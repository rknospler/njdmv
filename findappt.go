// findappt polls NJ MVC appointment pages for the earliest available
// "Initial Permit" slots across several locations, color-codes the
// results by urgency, and fires alerts (sound + email/SMS) when an
// opening appears within 3 days.
package main

import (
	"bufio"
	"fmt"
	"io"
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
	baseURL              = "https://telegov.njportal.com/njmvc/AppointmentWizard/11"
	bufferFile           = "/tmp/findappt_buffer.txt"
	alertCountFile       = "/tmp/findappt_alert_counts.txt"
	emailSentFile        = "/tmp/findappt_emails_sent.txt"
	maxBufferRuns        = 3
	maxConsecutiveAlerts = 8
	phoneEmail           = "9736705400@tmomail.net"
	httpTimeout          = 15 * time.Second
	colorGreen           = "\033[0;32m"
	colorYellow          = "\033[0;33m"
	colorRed             = "\033[0;31m"
	colorReset           = "\033[0m"
)

// Site is a single MVC location we monitor.
type Site struct {
	City   string // human-readable location name
	SiteID string // numeric ID used in the portal URL
}

// Appointment holds the scraped result for one location.
type Appointment struct {
	Epoch   int64  // unix timestamp of the earliest slot (0 = unavailable)
	SiteID  string // matches Site.SiteID
	City    string // matches Site.City
	RawDate string // date string as shown on the page, e.g. "March 5, 2026"
	URL     string // direct link to the booking page
}

var (
	// sites lists every MVC location we poll. Add or remove entries here
	// to change which offices are monitored.
	sites = []Site{
		{"Newton", "485"},
		{"Randolph", "123"},
		{"Newark", "116"},
		{"North Bergen", "117"},
		{"Rahway", "122"},
		{"Oakland", "119"},
		{"Washington", "486"},
		{"Lodi", "114"},
		{"Wayne", "118"},
	}

	// dateExtractRe captures the date from "Time of Appointment for <date>:".
	dateExtractRe = regexp.MustCompile(`Time of Appointment[[:space:]]*for[[:space:]]*([^:]+):`)

	// lineParseRe splits a CSV buffer line: siteID,city,date,url.
	lineParseRe = regexp.MustCompile(`^([^,]*),([^,]*),(.*),https:(.*)`)
)

func main() {
	results := fetchAllAppointmentsConcurrent()
	sortAppointmentsByDate(results)
	previous := loadPreviousRun()
	handleAlerts(results)

	currentTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("=== Run at %s ===\n", currentTime)
	displayResultsWithDiff(results, previous)
	fmt.Println()
	displayPreviousRuns()
	updateBuffer(currentTime, results)
}

// ---------------------------------------------------------------------------
// Concurrent Fetching
// ---------------------------------------------------------------------------

// fetchAllAppointmentsConcurrent spins up one goroutine per site and
// collects results into a fixed-size slice (no mutex needed because each
// goroutine writes to its own index).
func fetchAllAppointmentsConcurrent() []Appointment {
	var wg sync.WaitGroup
	results := make([]Appointment, len(sites))

	for i, site := range sites {
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
// available date. Returns a zero-epoch appointment on any network error.
func fetchAppointment(site Site) Appointment {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(buildURL(site.SiteID))
	if err != nil {
		return newAppointment(site, "", 0)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return newAppointment(site, "", 0)
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

// buildURL returns the full appointment-wizard URL for a given site.
func buildURL(siteID string) string {
	return fmt.Sprintf("%s/%s", baseURL, siteID)
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

	if !emailAlreadySent(key) {
		sendEmail(info)
		markEmailSent(key)
		fmt.Fprintf(os.Stderr, "📧 Email sent for this appointment\n")
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
	cmd := exec.Command("mail", "-s", "NJ MVC Appointment Available", phoneEmail)
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
func formatLine(appt Appointment) string {
	return fmt.Sprintf("%s,%s,%s,%s", appt.SiteID, appt.City, appt.RawDate, appt.URL)
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
// but are no longer present in the current site list.
func displayRemovedSites(current []Appointment, previous map[string]Appointment) {
	for siteID, prevAppt := range previous {
		if !siteExists(siteID, current) && prevAppt.RawDate != "" {
			line := fmt.Sprintf("%s,%s,%s,%s ❌ (site removed)",
				prevAppt.SiteID, prevAppt.City, prevAppt.RawDate, prevAppt.URL)
			fmt.Println(line)
		}
	}
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
