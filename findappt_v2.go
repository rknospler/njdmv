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

// Configuration
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

type Site struct {
	City   string
	SiteID string
}

type Appointment struct {
	Epoch   int64
	SiteID  string
	City    string
	RawDate string
	URL     string
}

var (
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
	dateExtractRe = regexp.MustCompile(`Time of Appointment[[:space:]]*for[[:space:]]*([^:]+):`)
	lineParseRe   = regexp.MustCompile(`^([^,]*),([^,]*),(.*),https:(.*)`)
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

// ============ Concurrent Fetching ============

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

// ============ Utilities ============

func newAppointment(site Site, rawDate string, epoch int64) Appointment {
	return Appointment{
		Epoch:   epoch,
		SiteID:  site.SiteID,
		City:    site.City,
		RawDate: rawDate,
		URL:     buildURL(site.SiteID),
	}
}

func buildURL(siteID string) string {
	return fmt.Sprintf("%s/%s", baseURL, siteID)
}

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

func extractDate(html string) string {
	if matches := dateExtractRe.FindStringSubmatch(html); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

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

func getColor(epoch int64) string {
	if epoch == 0 {
		return ""
	}
	apptTime := time.Unix(epoch, 0)
	now := time.Now()
	if apptTime.Before(now.Add(3 * 24 * time.Hour)) {
		return colorGreen
	}
	if apptTime.Before(now.Add(7 * 24 * time.Hour)) {
		return colorYellow
	}
	return colorRed
}

// ============ Alert System ============

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

func logAlertSuppressed(appt *Appointment) {
	info := fmt.Sprintf("%s on %s", appt.City, appt.RawDate)
	fmt.Fprintf(os.Stderr, "🔕 Alert limit reached for %s (suppressed)\n", info)
}

func generateAlertKey(appt *Appointment) string {
	return fmt.Sprintf("%s_%s", appt.SiteID, strings.ReplaceAll(appt.RawDate, " ", "_"))
}

func clearAlertTracking() {
	os.WriteFile(alertCountFile, []byte{}, 0644)
	os.WriteFile(emailSentFile, []byte{}, 0644)
}

func playSound() {
	exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Run()
}

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

// ============ State Management ============

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

func emailAlreadySent(key string) bool {
	data, err := os.ReadFile(emailSentFile)
	return err == nil && strings.Contains(string(data), key)
}

func markEmailSent(key string) {
	f, err := os.OpenFile(emailSentFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(key + "\n")
}

// ============ Display ============

func displayResultsWithDiff(current []Appointment, previous map[string]Appointment) {
	for _, appt := range current {
		line := formatLine(appt)
		marker := calculateDiffMarker(appt, previous)
		color := getColor(appt.Epoch)
		printLine(line, marker, color)
	}
	displayRemovedSites(current, previous)
}

func formatLine(appt Appointment) string {
	return fmt.Sprintf("%s,%s,%s,%s", appt.SiteID, appt.City, appt.RawDate, appt.URL)
}

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

func printLine(line, marker, color string) {
	if color != "" {
		fmt.Printf("%s%s%s%s\n", color, line, marker, colorReset)
	} else {
		fmt.Printf("%s%s\n", line, marker)
	}
}

func displayRemovedSites(current []Appointment, previous map[string]Appointment) {
	for siteID, prevAppt := range previous {
		if !siteExists(siteID, current) && prevAppt.RawDate != "" {
			line := fmt.Sprintf("%s,%s,%s,%s ❌ (site removed)",
				prevAppt.SiteID, prevAppt.City, prevAppt.RawDate, prevAppt.URL)
			fmt.Println(line)
		}
	}
}

func siteExists(siteID string, appointments []Appointment) bool {
	for _, appt := range appointments {
		if appt.SiteID == siteID {
			return true
		}
	}
	return false
}

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

// ============ Buffer Management ============

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
