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
	"time"
)

// Site represents a DMV location
type Site struct {
	City   string
	SiteID string
}

// Appointment represents an appointment with its details
type Appointment struct {
	Epoch   int64
	SiteID  string
	City    string
	RawDate string
	URL     string
}

const (
	baseURL              = "https://telegov.njportal.com/njmvc/AppointmentWizard/11"
	bufferFile           = "/tmp/findappt_buffer.txt"
	alertCountFile       = "/tmp/findappt_alert_counts.txt"
	emailSentFile        = "/tmp/findappt_emails_sent.txt"
	maxBufferRuns        = 3
	maxConsecutiveAlerts = 8
	phoneEmail           = "9736705400@tmomail.net"
)

// ANSI color codes
const (
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[0;33m"
	colorRed    = "\033[0;31m"
	colorReset  = "\033[0m"
)

var sites = []Site{
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

func main() {
	results := []Appointment{}

	// Fetch appointments for each site
	for _, site := range sites {
		appt := fetchAppointment(site)
		results = append(results, appt)
	}

	// Sort by epoch (earliest first)
	sort.Slice(results, func(i, j int) bool {
		if results[i].Epoch == 0 {
			return false
		}
		if results[j].Epoch == 0 {
			return true
		}
		return results[i].Epoch < results[j].Epoch
	})

	// Load previous run for comparison
	previousResults := loadPreviousRun()

	// Check for appointments within 3 days and handle alerts
	handleAlerts(results)

	// Display current run
	currentTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("=== Run at %s ===\n", currentTime)

	// Display results with colors and diff markers
	displayResultsWithDiff(results, previousResults)
	fmt.Println()

	// Display previous runs with colors
	displayPreviousRuns()

	// Update buffer
	updateBuffer(currentTime, results)
}

func newAppointment(site Site, rawDate string, epoch int64) Appointment {
	return Appointment{
		Epoch:   epoch,
		SiteID:  site.SiteID,
		City:    site.City,
		RawDate: rawDate,
		URL:     fmt.Sprintf("%s/%s", baseURL, site.SiteID),
	}
}

func fetchAppointment(site Site) Appointment {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/%s", baseURL, site.SiteID))
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

func extractDate(html string) string {
	// Look for "Time of Appointment for [date]:"
	re := regexp.MustCompile(`Time of Appointment[[:space:]]*for[[:space:]]*([^:]+):`)
	matches := re.FindStringSubmatch(html)

	if len(matches) > 1 {
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
	if apptTime.Before(time.Now().Add(3 * 24 * time.Hour)) {
		return colorGreen
	}
	if apptTime.Before(time.Now().Add(7 * 24 * time.Hour)) {
		return colorYellow
	}
	return colorRed
}

func handleAlerts(results []Appointment) {
	now := time.Now()
	threeDaysFromNow := now.Add(3 * 24 * time.Hour)

	var soonestAppt *Appointment
	for i := range results {
		if results[i].Epoch == 0 {
			continue
		}
		apptTime := time.Unix(results[i].Epoch, 0)
		if apptTime.Before(threeDaysFromNow) && apptTime.After(now) {
			soonestAppt = &results[i]
			break
		}
	}

	if soonestAppt == nil {
		// Clear tracking files
		os.WriteFile(alertCountFile, []byte{}, 0644)
		os.WriteFile(emailSentFile, []byte{}, 0644)
		return
	}

	key := fmt.Sprintf("%s_%s", soonestAppt.SiteID, strings.ReplaceAll(soonestAppt.RawDate, " ", "_"))
	currentCount := getAlertCount(key)

	if currentCount < maxConsecutiveAlerts {
		info := fmt.Sprintf("%s on %s - %s", soonestAppt.City, soonestAppt.RawDate, soonestAppt.URL)
		fmt.Fprintf(os.Stderr, "🔔 ALERT: Appointment available within 3 days! (Alert %d/%d)\n", currentCount+1, maxConsecutiveAlerts)
		fmt.Fprintf(os.Stderr, "%s\n", info)

		// Play sound
		exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Run()

		// Send email if not already sent
		if !emailAlreadySent(key) {
			sendEmail(info)
			markEmailSent(key)
			fmt.Fprintf(os.Stderr, "📧 Email sent for this appointment\n")
		}

		// Increment count
		incrementAlertCount(key, currentCount)
	} else {
		info := fmt.Sprintf("%s on %s", soonestAppt.City, soonestAppt.RawDate)
		fmt.Fprintf(os.Stderr, "🔕 Alert limit reached for %s (suppressed)\n", info)
	}
}

func getAlertCount(key string) int {
	data, err := os.ReadFile(alertCountFile)
	if err != nil {
		return 0
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
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
	if err != nil {
		return false
	}
	return strings.Contains(string(data), key)
}

func markEmailSent(key string) {
	f, err := os.OpenFile(emailSentFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(key + "\n")
}

func sendEmail(message string) {
	// Use mail command for simplicity (already configured)
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

func loadPreviousRun() map[string]Appointment {
	previous := make(map[string]Appointment)
	file, err := os.Open(bufferFile)
	if err != nil {
		return previous
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	inPreviousRun := false
	dateRe := regexp.MustCompile(`^([^,]*),([^,]*),(.*),https:(.*)`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=== Run at") {
			if inPreviousRun {
				break // Found the most recent previous run
			}
			inPreviousRun = true
			continue
		}
		if inPreviousRun && strings.Contains(line, "https") {
			if matches := dateRe.FindStringSubmatch(line); len(matches) > 4 {
				siteID := matches[1]
				city := matches[2]
				rawDate := strings.TrimSpace(matches[3])
				url := "https:" + matches[4]
				epoch := parseDate(rawDate)
				previous[siteID] = Appointment{
					Epoch:   epoch,
					SiteID:  siteID,
					City:    city,
					RawDate: rawDate,
					URL:     url,
				}
			}
		}
	}
	return previous
}

func displayResults(results []Appointment) {
	for _, appt := range results {
		line := fmt.Sprintf("%s,%s,%s,%s", appt.SiteID, appt.City, appt.RawDate, appt.URL)
		if color := getColor(appt.Epoch); color != "" {
			fmt.Printf("%s%s%s\n", color, line, colorReset)
		} else {
			fmt.Println(line)
		}
	}
}

func displayResultsWithDiff(current []Appointment, previous map[string]Appointment) {
	for _, appt := range current {
		line := fmt.Sprintf("%s,%s,%s,%s", appt.SiteID, appt.City, appt.RawDate, appt.URL)
		color := getColor(appt.Epoch)
		marker := ""

		// Check if this site existed in previous run
		if prevAppt, exists := previous[appt.SiteID]; exists {
			if appt.RawDate != prevAppt.RawDate {
				if appt.Epoch > 0 && prevAppt.Epoch > 0 {
					if appt.Epoch < prevAppt.Epoch {
						marker = " ⬇️  (earlier!)"
					} else {
						marker = " ⬆️  (later)"
					}
				} else if appt.RawDate != "" && prevAppt.RawDate == "" {
					marker = " ✨ (new!)"
				} else if appt.RawDate == "" && prevAppt.RawDate != "" {
					marker = " ❌ (removed)"
				}
			}
		} else if appt.RawDate != "" {
			marker = " ✨ (new!)"
		}

		if color != "" {
			fmt.Printf("%s%s%s%s\n", color, line, marker, colorReset)
		} else {
			fmt.Printf("%s%s\n", line, marker)
		}
	}

	// Check for sites that were removed
	for siteID, prevAppt := range previous {
		found := false
		for _, curr := range current {
			if curr.SiteID == siteID {
				found = true
				break
			}
		}
		if !found && prevAppt.RawDate != "" {
			line := fmt.Sprintf("%s,%s,%s,%s ❌ (site removed)", prevAppt.SiteID, prevAppt.City, prevAppt.RawDate, prevAppt.URL)
			fmt.Println(line)
		}
	}
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

	dateRe := regexp.MustCompile(`^[^,]*,[^,]*,(.*),https:`)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()

		// Headers and empty lines - print as-is
		if strings.HasPrefix(line, "===") || strings.TrimSpace(line) == "" {
			fmt.Println(line)
			continue
		}

		// Data lines - colorize if possible
		if matches := dateRe.FindStringSubmatch(line); len(matches) > 1 {
			epoch := parseDate(strings.TrimSpace(matches[1]))
			if color := getColor(epoch); color != "" {
				fmt.Printf("%s%s%s\n", color, line, colorReset)
				continue
			}
		}

		fmt.Println(line)
	}
}

func updateBuffer(currentTime string, results []Appointment) {
	// Read existing buffer
	existingData, _ := os.ReadFile(bufferFile)

	// Create new buffer content
	var buffer strings.Builder
	buffer.WriteString(fmt.Sprintf("=== Run at %s ===\n", currentTime))

	for _, appt := range results {
		buffer.WriteString(fmt.Sprintf("%s,%s,%s,%s\n", appt.SiteID, appt.City, appt.RawDate, appt.URL))
	}
	buffer.WriteString("\n")

	// Append existing buffer
	buffer.Write(existingData)

	// Limit to maxBufferRuns by counting "=== Run at" headers
	lines := strings.Split(buffer.String(), "\n")
	runCount := 0
	cutoffIndex := len(lines)

	for i, line := range lines {
		if strings.HasPrefix(line, "=== Run at") {
			runCount++
			if runCount > maxBufferRuns {
				cutoffIndex = i
				break
			}
		}
	}

	if cutoffIndex < len(lines) {
		lines = lines[:cutoffIndex]
	}

	// Write back
	os.WriteFile(bufferFile, []byte(strings.Join(lines, "\n")), 0644)
}
