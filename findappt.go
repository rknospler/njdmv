// findappt polls NJ MVC appointment pages for the earliest available
// slots across several locations, color-codes the results by urgency,
// and fires alerts (sound + email/SMS) when an opening appears within
// 3 days.
//
// Usage:
//
//	./findappt -all                                  # license renewal (default service)
//	./findappt -all -service registration             # registration renewal
//	./findappt -all -service realid                   # Real ID
//	./findappt -all -service title                    # new title/registration
//	./findappt -zip 07869 -radius 25                 # search within 25 mi of zip
//	./findappt -all -notify 5551234567@tmomail.net   # also send SMS alerts
//	./findappt -list                                 # list locations for service
//	./findappt -services                             # list all service types
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	portalBase           = "https://telegov.njportal.com/njmvc/AppointmentWizard"
	bufferFile           = "/tmp/findappt_buffer.txt"
	alertCountFile       = "/tmp/findappt_alert_counts.txt"
	emailSentFile        = "/tmp/findappt_emails_sent.txt"
	maxBufferRuns        = 3
	maxConsecutiveAlerts = 8
	httpTimeout          = 15 * time.Second
	defaultRadius        = 30.0 // miles
	colorGreen           = "\033[0;32m"
	colorYellow          = "\033[0;33m"
	colorRed             = "\033[0;31m"
	colorCyan            = "\033[0;36m"
	colorReset           = "\033[0m"
)

// ServiceType maps a user-friendly name to the portal path number.
type ServiceType struct {
	Name   string // short CLI name
	Label  string // human-readable description
	PathID string // the number in /AppointmentWizard/<PathID>
}

// serviceTypes is the catalog of supported appointment types.
var serviceTypes = []ServiceType{
	{"renewal", "License / Non-Driver ID Renewal", "11"},
	{"realid", "Real ID", "12"},
	{"permit", "Initial Permit (before knowledge test)", "15"},
	{"title", "New Title or Registration", "8"},
	{"registration", "Registration Renewal", "10"},
	{"replace-title", "Replacement Title", "13"},
	{"transfer", "Transfer from Out of State", "7"},
	{"cdl-permit", "CDL Permit or Endorsement", "14"},
	{"non-driver", "Non-Driver ID (new)", "16"},
	{"knowledge", "Knowledge Test (non-CDL)", "19"},
	{"cdl-knowledge", "CDL Knowledge Test", "20"},
}

// Site is a single MVC location we monitor.
type Site struct {
	City   string  // human-readable location name (e.g. "Bakers Basin")
	SiteID string  // numeric ID used in the portal URL (varies by service!)
	Lat    float64 // latitude (decimal degrees)
	Lon    float64 // longitude (decimal degrees)
	Zip    string  // zip code of the office
}

// Appointment holds the scraped result for one location.
type Appointment struct {
	Epoch    int64  // unix timestamp of the earliest slot (0 = unavailable)
	SiteID   string // matches Site.SiteID
	City     string // matches Site.City
	RawDate  string // date string as shown on the page, e.g. "March 5, 2026"
	URL      string // direct link to the booking page
	FetchErr string // non-empty if the request failed (timeout, blocked, etc.)
}

// gpsLookup maps a normalized zip code prefix (first 5 digits) to
// GPS coordinates. This table covers every zip seen across all MVC
// service types so that we never need a network geocode call.
var gpsLookup = map[string][2]float64{
	"08648": {40.2932, -74.7399}, // Bakers Basin / Lawrenceville
	"07002": {40.6687, -74.1143}, // Bayonne
	"08104": {39.9260, -75.1196}, // Camden
	"08234": {39.3940, -74.6101}, // Cardiff / Egg Harbor Twp
	"08075": {40.0517, -74.9542}, // Delanco
	"07724": {40.2960, -74.0510}, // Eatontown
	"08817": {40.5187, -74.4121}, // Edison
	"07201": {40.6640, -74.2107}, // Elizabeth
	"08551": {40.4464, -74.8365}, // Flemington
	"07728": {40.2601, -74.2774}, // Freehold
	"07644": {40.8823, -74.0832}, // Lodi
	"08050": {39.6951, -74.2588}, // Manahawkin
	"07114": {40.7178, -74.1712}, // Newark
	"07860": {41.0581, -74.7526}, // Newton
	"07047": {40.8040, -74.0121}, // North Bergen
	"07436": {41.0135, -74.2643}, // Oakland
	"07505": {40.9168, -74.1719}, // Paterson
	"07065": {40.6080, -74.2782}, // Rahway
	"07869": {40.8485, -74.5866}, // Randolph
	"08204": {38.9818, -74.9579}, // Rio Grande
	"08078": {39.8519, -75.0672}, // Runnemede
	"08079": {39.5718, -75.4709}, // Salem
	"07080": {40.5759, -74.4115}, // South Plainfield
	"08753": {39.9712, -74.1769}, // Toms River
	"08360": {39.4863, -75.0259}, // Vineland
	"07882": {40.7587, -74.9793}, // Washington
	"07470": {40.9251, -74.2765}, // Wayne
	"08086": {39.8518, -75.1807}, // West Deptford
	// Registration / title-only offices:
	"08002": {39.9348, -75.0307}, // Cherry Hill
	"07018": {40.7644, -74.2150}, // East Orange
	"07730": {40.4155, -74.1857}, // Hazlet
	"07307": {40.7489, -74.0565}, // Jersey City
	"08701": {40.0879, -74.2066}, // Lakewood
	"08055": {39.8688, -74.8236}, // Medford
	"08876": {40.5740, -74.6099}, // Somerville
	"08810": {40.3862, -74.5333}, // South Brunswick
	"07081": {40.6989, -74.3215}, // Springfield
	"08666": {40.2206, -74.7698}, // Trenton Regional
	"08012": {39.7718, -75.0521}, // Turnersville
	"07057": {40.8534, -74.1070}, // Wallington
}

var (
	// activeSites holds the locations selected for this run (set in main).
	activeSites []Site

	// allSites is populated at startup from the portal's locationData JSON
	// for the selected service type.
	allSites []Site

	// activeService is the selected service (set in main).
	activeService ServiceType

	// notifyAddr is the email/SMS gateway address for alerts (empty = no email).
	notifyAddr string

	// dateExtractRe captures the date from "Time of Appointment for <date>:".
	dateExtractRe = regexp.MustCompile(`Time of Appointment[[:space:]]*for[[:space:]]*([^:]+):`)

	// locationDataRe extracts the locationData JSON array from the index page.
	locationDataRe = regexp.MustCompile(`var locationData\s*=\s*(\[.*?\]);`)

	// lineParseRe splits a CSV buffer line: siteID,city,date,url.
	lineParseRe = regexp.MustCompile(`^([^,]*),([^,]*),(.*),https:(.*)`)

	// originLat/originLon are set when --zip is used, for display purposes.
	originLat, originLon float64
	useZipFilter         bool

	// runParams summarizes the CLI flags used for this run, stored in the buffer.
	runParams string
)

func main() {
	zip := flag.String("zip", "", "center zip code for radius search")
	radius := flag.Float64("radius", defaultRadius, "search radius in miles (used with -zip)")
	all := flag.Bool("all", false, "search all locations for this service")
	list := flag.Bool("list", false, "print locations for this service and exit")
	notify := flag.String("notify", "", "email/SMS gateway address for alerts (e.g. 5551234567@tmomail.net)")
	service := flag.String("service", "renewal", "service type (use -services to list)")
	showServices := flag.Bool("services", false, "list all available service types and exit")
	flag.Parse()

	// --services: print the service catalog and exit.
	if *showServices {
		printServiceTypes()
		return
	}

	// Look up the requested service type.
	svc, ok := lookupService(*service)
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown service %q. Use -services to see available types.\n", *service)
		os.Exit(1)
	}
	activeService = svc

	// Fetch the service's index page to discover locations + site IDs.
	var err error
	allSites, err = discoverSites(activeService)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not load locations for %s: %v\n", activeService.Label, err)
		os.Exit(1)
	}

	// --list: dump the location table for the selected service and exit.
	if *list {
		fmt.Fprintf(os.Stderr, "Service: %s\n\n", activeService.Label)
		printAllLocations()
		return
	}

	notifyAddr = *notify

	// Require either -zip or -all.
	if *zip == "" && !*all {
		fmt.Fprintf(os.Stderr, "Usage: findappt -all [-service <type>] [-notify <addr>]\n")
		fmt.Fprintf(os.Stderr, "       findappt -zip <zipcode> [-radius <miles>] [-service <type>] [-notify <addr>]\n")
		fmt.Fprintf(os.Stderr, "       findappt -list [-service <type>]\n")
		fmt.Fprintf(os.Stderr, "       findappt -services\n\n")
		fmt.Fprintf(os.Stderr, "Services:\n")
		for _, s := range serviceTypes {
			fmt.Fprintf(os.Stderr, "  %-16s %s\n", s.Name, s.Label)
		}
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "🔧 Service: %s\n", activeService.Label)

	// Determine which sites to scan.
	switch {
	case *zip != "":
		originLat, originLon, err = geocodeZip(*zip)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not geocode zip %q: %v\n", *zip, err)
			os.Exit(1)
		}
		useZipFilter = true
		activeSites = filterByRadius(originLat, originLon, *radius)
		if len(activeSites) == 0 {
			fmt.Fprintf(os.Stderr, "No MVC locations within %.0f miles of %s\n", *radius, *zip)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "📍 Searching %d locations within %.0f miles of %s\n\n", len(activeSites), *radius, *zip)
		runParams = fmt.Sprintf("-service %s -zip %s -radius %.0f", activeService.Name, *zip, *radius)
	case *all:
		activeSites = allSites
		fmt.Fprintf(os.Stderr, "📍 Searching all %d locations\n\n", len(activeSites))
		runParams = fmt.Sprintf("-service %s -all", activeService.Name)
	}

	results := fetchAllAppointmentsConcurrent()
	reportFetchErrors(results)
	sortAppointmentsByDate(results)
	previous := loadPreviousRun()
	handleAlerts(results)

	currentTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("=== Run at %s ===\n", currentTime)
	fmt.Printf("    %s\n", runParams)
	displayResultsWithDiff(results, previous)
	fmt.Println()
	displayPreviousRuns()
	updateBuffer(currentTime, results)
}

// lookupService returns the ServiceType matching the given name.
func lookupService(name string) (ServiceType, bool) {
	for _, s := range serviceTypes {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return ServiceType{}, false
}

// printServiceTypes lists all known service types to stdout.
func printServiceTypes() {
	fmt.Printf("%-16s  %s\n", "Name", "Description")
	fmt.Println(strings.Repeat("─", 55))
	for _, s := range serviceTypes {
		fmt.Printf("%-16s  %s\n", s.Name, s.Label)
	}
}

// discoverSites fetches the service's index page from the NJ MVC portal,
// parses the embedded locationData JSON, and returns the list of Sites
// with their service-specific site IDs and GPS coordinates.
func discoverSites(svc ServiceType) ([]Site, error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(fmt.Sprintf("%s/%s", portalBase, svc.PathID))
	if err != nil {
		return nil, fmt.Errorf("fetch index: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	matches := locationDataRe.FindSubmatch(body)
	if matches == nil {
		return nil, fmt.Errorf("locationData not found in page")
	}

	var locations []struct {
		Name string `json:"Name"`
		Zip  string `json:"Zip"`
		Loc  []struct {
			LocationId int `json:"LocationId"`
		} `json:"LocAppointments"`
	}
	if err := json.Unmarshal(matches[1], &locations); err != nil {
		return nil, fmt.Errorf("parse locationData: %w", err)
	}

	var sites []Site
	for _, loc := range locations {
		if len(loc.Loc) == 0 {
			continue
		}
		city := loc.Name
		if idx := strings.Index(city, " - "); idx > 0 {
			city = strings.TrimSpace(city[:idx])
		}
		// Also handle the " -" variant (some entries have no space before dash).
		if idx := strings.Index(city, " -"); idx > 0 {
			city = strings.TrimSpace(city[:idx])
		}

		zip5 := loc.Zip
		if len(zip5) > 5 {
			zip5 = zip5[:5] // e.g. "08234-3935" → "08234"
		}

		var lat, lon float64
		if coords, ok := gpsLookup[zip5]; ok {
			lat, lon = coords[0], coords[1]
		} else {
			// Fallback: geocode from zip (shouldn't normally happen).
			lat, lon, _ = geocodeZip(zip5)
		}

		sites = append(sites, Site{
			City:   city,
			SiteID: fmt.Sprintf("%d", loc.Loc[0].LocationId),
			Lat:    lat,
			Lon:    lon,
			Zip:    zip5,
		})
	}

	if len(sites) == 0 {
		return nil, fmt.Errorf("no locations found")
	}
	return sites, nil
}

// printAllLocations dumps every known MVC office to stdout.
func printAllLocations() {
	fmt.Printf("%-5s  %-20s  %-8s  %8s  %9s\n", "ID", "City", "Zip", "Lat", "Lon")
	fmt.Println(strings.Repeat("─", 60))
	for _, s := range allSites {
		fmt.Printf("%-5s  %-20s  %-8s  %8.4f  %9.4f\n",
			s.SiteID, s.City, s.Zip, s.Lat, s.Lon)
	}
	fmt.Printf("\n%d locations total\n", len(allSites))
}

// ---------------------------------------------------------------------------
// Geocoding & Distance
// ---------------------------------------------------------------------------

// geocodeZip resolves a US zip code to lat/lon using the free
// zippopotam.us API (no key required).
func geocodeZip(zip string) (float64, float64, error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get("https://api.zippopotam.us/us/" + zip)
	if err != nil {
		return 0, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("zip code not found (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Places []struct {
			Lat string `json:"latitude"`
			Lon string `json:"longitude"`
		} `json:"places"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, fmt.Errorf("decode failed: %w", err)
	}
	if len(result.Places) == 0 {
		return 0, 0, fmt.Errorf("no results for zip %s", zip)
	}

	var lat, lon float64
	fmt.Sscanf(result.Places[0].Lat, "%f", &lat)
	fmt.Sscanf(result.Places[0].Lon, "%f", &lon)
	return lat, lon, nil
}

// haversine returns the great-circle distance in miles between two
// points given in decimal degrees.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMiles = 3958.8
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	la1 := lat1 * math.Pi / 180
	la2 := lat2 * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(la1)*math.Cos(la2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadiusMiles * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// filterByRadius returns all sites within the given radius (miles)
// from the origin point, sorted by distance.
func filterByRadius(lat, lon, radius float64) []Site {
	type ranked struct {
		site Site
		dist float64
	}
	var matches []ranked
	for _, s := range allSites {
		d := haversine(lat, lon, s.Lat, s.Lon)
		if d <= radius {
			matches = append(matches, ranked{s, d})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].dist < matches[j].dist
	})
	out := make([]Site, len(matches))
	for i, m := range matches {
		out[i] = m.site
	}
	return out
}

// ---------------------------------------------------------------------------
// Concurrent Fetching
// ---------------------------------------------------------------------------

// fetchAllAppointmentsConcurrent spins up one goroutine per site and
// collects results into a fixed-size slice (no mutex needed because each
// goroutine writes to its own index).
func fetchAllAppointmentsConcurrent() []Appointment {
	var wg sync.WaitGroup
	results := make([]Appointment, len(activeSites))

	for i, site := range activeSites {
		wg.Add(1)
		go func(index int, s Site) {
			defer wg.Done()
			results[index] = fetchAppointment(s)
		}(i, site)
	}

	wg.Wait()
	return results
}

// fetchAppointment downloads one location's page and extracts the earliest
// available date. Returns a zero-epoch appointment on any network error,
// recording the failure reason in FetchErr.
func fetchAppointment(site Site) Appointment {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(buildURL(site.SiteID))
	if err != nil {
		appt := newAppointment(site, "", 0)
		appt.FetchErr = fmt.Sprintf("request failed: %v", err)
		return appt
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		appt := newAppointment(site, "", 0)
		appt.FetchErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return appt
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		appt := newAppointment(site, "", 0)
		appt.FetchErr = fmt.Sprintf("read body: %v", err)
		return appt
	}

	rawDate := extractDate(string(body))
	return newAppointment(site, rawDate, parseDate(rawDate))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newAppointment is a convenience constructor.
func newAppointment(site Site, rawDate string, epoch int64) Appointment {
	return Appointment{
		Epoch:   epoch,
		SiteID:  site.SiteID,
		City:    site.City,
		RawDate: rawDate,
		URL:     buildURL(site.SiteID),
	}
}

// reportFetchErrors prints a summary of any locations that failed to respond.
func reportFetchErrors(results []Appointment) {
	var failures []Appointment
	for _, r := range results {
		if r.FetchErr != "" {
			failures = append(failures, r)
		}
	}
	if len(failures) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "⚠️  %d/%d locations failed to respond:\n", len(failures), len(results))
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "   %s (%s): %s\n", f.City, f.SiteID, f.FetchErr)
	}
	fmt.Fprintln(os.Stderr)
}

// buildURL returns the full appointment-wizard URL for a given site,
// using the active service's path.
func buildURL(siteID string) string {
	return fmt.Sprintf("%s/%s/%s", portalBase, activeService.PathID, siteID)
}

// sortAppointmentsByDate orders appointments soonest-first, pushing
// unavailable (epoch == 0) entries to the end.
func sortAppointmentsByDate(results []Appointment) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].Epoch == 0 {
			return false
		}
		if results[j].Epoch == 0 {
			return true
		}
		return results[i].Epoch < results[j].Epoch
	})
}

// extractDate pulls the appointment date out of the raw HTML.
func extractDate(html string) string {
	if matches := dateExtractRe.FindStringSubmatch(html); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

// parseDate converts a human-readable date like "March 5, 2026" to a
// unix timestamp. Returns 0 if the string is empty or unparseable.
func parseDate(dateStr string) int64 {
	if dateStr == "" {
		return 0
	}
	for _, layout := range []string{"January 2, 2006", "January 02, 2006"} {
		if t, err := time.Parse(layout, dateStr); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// getColor returns an ANSI escape for urgency:
//
//	green  = within 3 days
//	yellow = within 7 days
//	red    = 7+ days out
//	""     = no date available
func getColor(epoch int64) string {
	if epoch == 0 {
		return ""
	}

	apptTime := time.Unix(epoch, 0)
	now := time.Now()

	switch {
	case apptTime.Before(now.Add(3 * 24 * time.Hour)):
		return colorGreen
	case apptTime.Before(now.Add(7 * 24 * time.Hour)):
		return colorYellow
	default:
		return colorRed
	}
}

// ---------------------------------------------------------------------------
// Alert System
// ---------------------------------------------------------------------------

// handleAlerts checks whether the soonest appointment is within 3 days
// and, if so, plays a sound and sends an email/SMS (up to
// maxConsecutiveAlerts times per unique slot).
func handleAlerts(results []Appointment) {
	soonestAppt := findSoonestAppointment(results)
	if soonestAppt == nil {
		clearAlertTracking()
		return
	}

	key := generateAlertKey(soonestAppt)
	currentCount := getAlertCount(key)

	if currentCount < maxConsecutiveAlerts {
		triggerAlert(soonestAppt, key, currentCount)
	} else {
		logAlertSuppressed(soonestAppt)
	}
}

// findSoonestAppointment returns the first appointment (already sorted)
// that falls between now and 3 days from now, or nil.
func findSoonestAppointment(results []Appointment) *Appointment {
	now := time.Now()
	threshold := now.Add(3 * 24 * time.Hour)

	for i := range results {
		if results[i].Epoch == 0 {
			continue
		}
		apptTime := time.Unix(results[i].Epoch, 0)
		if apptTime.Before(threshold) && apptTime.After(now) {
			return &results[i]
		}
	}
	return nil
}

// triggerAlert plays the alert sound, prints to stderr, and sends an
// email/SMS the first time a new appointment key is seen.
func triggerAlert(appt *Appointment, key string, count int) {
	info := fmt.Sprintf("%s on %s - %s", appt.City, appt.RawDate, appt.URL)
	fmt.Fprintf(os.Stderr, "🔔 ALERT: Appointment available within 3 days! (Alert %d/%d)\n", count+1, maxConsecutiveAlerts)
	fmt.Fprintf(os.Stderr, "%s\n", info)

	playSound()

	if notifyAddr != "" && !emailAlreadySent(key) {
		sendEmail(info)
		markEmailSent(key)
		fmt.Fprintf(os.Stderr, "📧 Email sent to %s\n", notifyAddr)
	}

	incrementAlertCount(key, count)
}

// logAlertSuppressed prints a notice when alerts are capped.
func logAlertSuppressed(appt *Appointment) {
	info := fmt.Sprintf("%s on %s", appt.City, appt.RawDate)
	fmt.Fprintf(os.Stderr, "🔕 Alert limit reached for %s (suppressed)\n", info)
}

// generateAlertKey builds a dedup key like "485_March_5,_2026".
func generateAlertKey(appt *Appointment) string {
	return fmt.Sprintf("%s_%s", appt.SiteID, strings.ReplaceAll(appt.RawDate, " ", "_"))
}

// clearAlertTracking resets persisted alert state when no imminent
// appointments exist.
func clearAlertTracking() {
	os.WriteFile(alertCountFile, []byte{}, 0644)
	os.WriteFile(emailSentFile, []byte{}, 0644)
}

// playSound plays the macOS Glass sound. No-op if afplay isn't available.
func playSound() {
	exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Run()
}

// sendEmail pipes a message into the mail command, which is expected to
// relay to the T-Mobile SMS-to-email gateway.
func sendEmail(message string) {
	cmd := exec.Command("mail", "-s", "NJ MVC Appointment Available", notifyAddr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	go func() {
		defer stdin.Close()
		io.WriteString(stdin, message)
	}()
	cmd.Run()
}

// ---------------------------------------------------------------------------
// State Management (flat-file persistence in /tmp)
// ---------------------------------------------------------------------------

// getAlertCount reads the current consecutive-alert count for a key.
func getAlertCount(key string) int {
	data, err := os.ReadFile(alertCountFile)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key+"=") {
			var count int
			fmt.Sscanf(line, key+"=%d", &count)
			return count
		}
	}
	return 0
}

// incrementAlertCount bumps the persisted counter for key.
func incrementAlertCount(key string, currentCount int) {
	data, _ := os.ReadFile(alertCountFile)
	var newLines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, key+"=") {
			newLines = append(newLines, line)
		}
	}
	newLines = append(newLines, fmt.Sprintf("%s=%d", key, currentCount+1))
	os.WriteFile(alertCountFile, []byte(strings.Join(newLines, "\n")), 0644)
}

// emailAlreadySent returns true if we already emailed for this key.
func emailAlreadySent(key string) bool {
	data, err := os.ReadFile(emailSentFile)
	return err == nil && strings.Contains(string(data), key)
}

// markEmailSent appends the key so future runs skip the email.
func markEmailSent(key string) {
	f, err := os.OpenFile(emailSentFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(key + "\n")
}

// ---------------------------------------------------------------------------
// Display
// ---------------------------------------------------------------------------

// displayResultsWithDiff prints the current run, annotating each line
// with a diff marker when the date changed compared to the previous run.
func displayResultsWithDiff(current []Appointment, previous map[string]Appointment) {
	for _, appt := range current {
		line := formatLine(appt)
		marker := calculateDiffMarker(appt, previous)
		color := getColor(appt.Epoch)
		printLine(line, marker, color)
	}
	displayRemovedSites(current, previous)
}

// formatLine renders one appointment as a CSV-style string.
// When running with --zip, it appends the distance from the origin.
func formatLine(appt Appointment) string {
	base := fmt.Sprintf("%s,%s,%s,%s", appt.SiteID, appt.City, appt.RawDate, appt.URL)
	if useZipFilter {
		// Look up site coordinates for distance display.
		for _, s := range activeSites {
			if s.SiteID == appt.SiteID {
				d := haversine(originLat, originLon, s.Lat, s.Lon)
				return fmt.Sprintf("%s  %s(%.0f mi)%s", base, colorCyan, d, colorReset)
			}
		}
	}
	return base
}

// calculateDiffMarker compares the current appointment against the
// previous run and returns an emoji marker:
//
//	⬇️  earlier!  – date moved closer
//	⬆️  later     – date moved further out
//	✨ new!       – slot appeared where none existed
//	❌ removed    – slot disappeared
func calculateDiffMarker(current Appointment, previous map[string]Appointment) string {
	prevAppt, exists := previous[current.SiteID]
	if !exists {
		if current.RawDate != "" {
			return " ✨ (new!)"
		}
		return ""
	}

	if current.RawDate == prevAppt.RawDate {
		return ""
	}

	if current.Epoch > 0 && prevAppt.Epoch > 0 {
		if current.Epoch < prevAppt.Epoch {
			return " ⬇️  (earlier!)"
		}
		return " ⬆️  (later)"
	}

	if current.RawDate != "" && prevAppt.RawDate == "" {
		return " ✨ (new!)"
	}
	if current.RawDate == "" && prevAppt.RawDate != "" {
		return " ❌ (removed)"
	}
	return ""
}

// printLine writes a single result line, optionally wrapped in an ANSI
// color and appended with a diff marker.
func printLine(line, marker, color string) {
	if color != "" {
		fmt.Printf("%s%s%s%s\n", color, line, marker, colorReset)
	} else {
		fmt.Printf("%s%s\n", line, marker)
	}
}

// displayRemovedSites prints entries that existed in the previous run
// but are no longer present in the current results. Only considers sites
// that are in the current search scope (activeSites) to avoid false
// "removed" markers when search parameters change between runs.
func displayRemovedSites(current []Appointment, previous map[string]Appointment) {
	for siteID, prevAppt := range previous {
		if !siteExists(siteID, current) && prevAppt.RawDate != "" && siteInScope(siteID) {
			line := fmt.Sprintf("%s,%s,%s,%s ❌ (removed)",
				prevAppt.SiteID, prevAppt.City, prevAppt.RawDate, prevAppt.URL)
			fmt.Println(line)
		}
	}
}

// siteInScope returns true if siteID is in the current activeSites list.
func siteInScope(siteID string) bool {
	for _, s := range activeSites {
		if s.SiteID == siteID {
			return true
		}
	}
	return false
}

// siteExists returns true if siteID appears anywhere in the slice.
func siteExists(siteID string, appointments []Appointment) bool {
	for _, appt := range appointments {
		if appt.SiteID == siteID {
			return true
		}
	}
	return false
}

// displayPreviousRuns replays the buffer file, re-colorizing each line
// based on how far out its date is *now*.
func displayPreviousRuns() {
	file, err := os.Open(bufferFile)
	if err != nil {
		return
	}
	defer file.Close()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Previous Runs:")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		printBufferLine(scanner.Text())
	}
}

// printBufferLine colorizes a single line from the buffer file.
func printBufferLine(line string) {
	if strings.HasPrefix(line, "===") || strings.TrimSpace(line) == "" {
		fmt.Println(line)
		return
	}

	if matches := lineParseRe.FindStringSubmatch(line); len(matches) > 1 {
		epoch := parseDate(strings.TrimSpace(matches[3]))
		if color := getColor(epoch); color != "" {
			fmt.Printf("%s%s%s\n", color, line, colorReset)
			return
		}
	}
	fmt.Println(line)
}

// ---------------------------------------------------------------------------
// Buffer Management — keeps a rolling log of the last N runs in a
// flat text file so we can diff against the previous run and show
// history.
// ---------------------------------------------------------------------------

// loadPreviousRun reads the most recent run from the buffer file and
// returns a map keyed by SiteID for quick diff lookup.
func loadPreviousRun() map[string]Appointment {
	previous := make(map[string]Appointment)
	file, err := os.Open(bufferFile)
	if err != nil {
		return previous
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	inPreviousRun := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=== Run at") {
			if inPreviousRun {
				break
			}
			inPreviousRun = true
			continue
		}
		if inPreviousRun {
			if appt := parseAppointmentLine(line); appt != nil {
				previous[appt.SiteID] = *appt
			}
		}
	}
	return previous
}

// parseAppointmentLine converts a CSV buffer line back into an
// Appointment, or nil if the line is not a data row.
func parseAppointmentLine(line string) *Appointment {
	if !strings.Contains(line, "https") {
		return nil
	}

	matches := lineParseRe.FindStringSubmatch(line)
	if len(matches) <= 4 {
		return nil
	}

	rawDate := strings.TrimSpace(matches[3])
	return &Appointment{
		Epoch:   parseDate(rawDate),
		SiteID:  matches[1],
		City:    matches[2],
		RawDate: rawDate,
		URL:     "https:" + matches[4],
	}
}

// updateBuffer prepends the current run to the buffer file and trims
// it to maxBufferRuns entries.
func updateBuffer(currentTime string, results []Appointment) {
	existingData, _ := os.ReadFile(bufferFile)

	var buffer strings.Builder
	buffer.WriteString(fmt.Sprintf("=== Run at %s ===\n", currentTime))
	buffer.WriteString(fmt.Sprintf("    %s\n", runParams))
	for _, appt := range results {
		buffer.WriteString(fmt.Sprintf("%s,%s,%s,%s\n", appt.SiteID, appt.City, appt.RawDate, appt.URL))
	}
	buffer.WriteString("\n")
	buffer.Write(existingData)

	lines := limitBufferToRuns(buffer.String(), maxBufferRuns)
	os.WriteFile(bufferFile, []byte(strings.Join(lines, "\n")), 0644)
}

// limitBufferToRuns truncates buffer content to at most maxRuns entries.
func limitBufferToRuns(content string, maxRuns int) []string {
	lines := strings.Split(content, "\n")
	runCount := 0
	cutoffIndex := len(lines)

	for i, line := range lines {
		if strings.HasPrefix(line, "=== Run at") {
			runCount++
			if runCount > maxRuns {
				cutoffIndex = i
				break
			}
		}
	}

	if cutoffIndex < len(lines) {
		return lines[:cutoffIndex]
	}
	return lines
}
