package watershed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ses"
)

const (
	baseURL      = "https://monitormywatershed.org/dataloader/ajax/"
	samplingCode = "MSPL2S"
)

// Move thresholds to be configurable
type ThresholdConfig struct {
	max float64
	min float64
}

func getThresholdFromEnv(measurement string) ThresholdConfig {
	maxKey := fmt.Sprintf("%s_MAX", measurement)
	minKey := fmt.Sprintf("%s_MIN", measurement)

	// Get values from environment with defaults
	maxStr := os.Getenv(maxKey)
	minStr := os.Getenv(minKey)

	// Default values matching our original thresholds
	defaults := map[string]ThresholdConfig{
		"WATER_DEPTH":             {max: 1000, min: 0},
		"TEMPERATURE":             {max: 26, min: -20},
		"ELECTRICAL_CONDUCTIVITY": {max: 600, min: 0},
		"TURBIDITY":               {max: 150, min: 0},
		"BATTERY_VOLTAGE":         {max: 5, min: 0},
		"PERCENT_FULL_SCALE":      {max: 101, min: 0},
		"RELATIVE_HUMIDITY":       {max: 100, min: 0},
	}

	default_config := defaults[measurement]

	// Parse environment variables if present, otherwise use defaults
	max := default_config.max
	if maxStr != "" {
		if parsed, err := strconv.ParseFloat(maxStr, 64); err == nil {
			max = parsed
		}
	}

	min := default_config.min
	if minStr != "" {
		if parsed, err := strconv.ParseFloat(minStr, 64); err == nil {
			min = parsed
		}
	}

	return ThresholdConfig{max: max, min: min}
}

type Monitor struct {
	s3Client      *s3.S3
	bucketName    string
	lastEmailSent map[string]time.Time
}

func NewMonitor() *Monitor {
	sess := session.Must(session.NewSession())
	return &Monitor{
		s3Client:      s3.New(sess),
		bucketName:    "watershed-monitor-state",
		lastEmailSent: make(map[string]time.Time),
	}
}

type RequestData struct {
	Method              string `json:"method"`
	SamplingFeatureCode string `json:"sampling_feature_code,omitempty"`
	ResultID            string `json:"resultid,omitempty"`
	StartDate           string `json:"start_date,omitempty"`
	EndDate             string `json:"end_date,omitempty"`
}

type TimeSeriesData struct {
	ValueID             map[string]int64   `json:"valueid"`
	DataValue           map[string]float64 `json:"datavalue"`
	ValueDateTime       map[string]int64   `json:"valuedatetime"`
	ValueDateTimeOffset map[string]int     `json:"valuedatetimeutcoffset"`
}

func (m *Monitor) sendEmailSES(subject, body string) error {
	// Add debug logging
	fmt.Printf("Attempting to send email:\nFrom: %s\nTo: %s\nSubject: %s\n",
		os.Getenv("SES_FROM_ADDRESS"),
		os.Getenv("EMAIL_RECIPIENT"),
		subject)

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION")),
	})
	if err != nil {
		return fmt.Errorf("session error: %v", err)
	}

	svc := ses.New(sess)
	input := &ses.SendEmailInput{
		Destination: &ses.Destination{
			ToAddresses: []*string{
				aws.String(os.Getenv("EMAIL_RECIPIENT")),
			},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Text: &ses.Content{
					Data: aws.String(body),
				},
			},
			Subject: &ses.Content{
				Data: aws.String(subject),
			},
		},
		Source: aws.String(os.Getenv("SES_FROM_ADDRESS")),
	}

	result, err := svc.SendEmail(input)
	if err != nil {
		return fmt.Errorf("send error: %v", err)
	}
	fmt.Printf("Email sent! Message ID: %s\n", *result.MessageId)
	return nil
}

func (m *Monitor) getLastEmailTime(name string) (time.Time, bool) {
	lastSent, exists := m.lastEmailSent[name]
	return lastSent, exists
}

func (m *Monitor) updateLastEmailTime(name string, t time.Time) error {
	m.lastEmailSent[name] = t
	return nil
}

func (m *Monitor) checkAndNotify(name string, value float64, timestamp time.Time) {
	envName := strings.ReplaceAll(strings.ToUpper(name), " ", "_")
	threshold := getThresholdFromEnv(envName)

	if value >= threshold.max || value < threshold.min {
		fmt.Printf("\nALERT: %s value %.2f is outside acceptable range (%.2f to %.2f)\n",
			name, value, threshold.min, threshold.max)

		// Check for last email time
		if lastSent, exists := m.getLastEmailTime(name); exists {
			timeSince := time.Since(lastSent)
			if timeSince < 12*time.Hour {
				hoursLeft := 12 - timeSince.Hours()
				fmt.Printf("Notice: Email alert suppressed - previous alert was sent %.1f hours ago (waiting %.1f more hours)\n",
					timeSince.Hours(), hoursLeft)
				return
			}
		}

		// Only attempt email if configured
		if os.Getenv("EMAIL_RECIPIENT") == "" {
			fmt.Println("Notice: Email alert suppressed - no email recipient configured")
			return
		}

		// Format timestamp in EST
		estTime := timestamp.Add(-5 * time.Hour)
		timeStr := estTime.Format("2006-01-02 15:04:05 EST")

		subject := fmt.Sprintf("%s Alert", name)
		body := fmt.Sprintf("%s has reached %.2f at %s\n(Acceptable range: %.2f to %.2f)",
			name, value, timeStr, threshold.min, threshold.max)

		if err := m.sendEmailSES(subject, body); err != nil {
			fmt.Printf("Error sending email for %s: %v\n", name, err)
			return // Don't update last email time if sending failed
		}

		// Update with new time
		if err := m.updateLastEmailTime(name, time.Now()); err != nil {
			fmt.Printf("Warning: Failed to update last email time: %v\n", err)
			return
		}
		fmt.Printf("Alert email sent for %s (value: %.2f)\n", name, value)
	}
}

func (m *Monitor) makePostRequest(urlStr string, formValues map[string]string) ([]byte, error) {
	formData := make(url.Values)
	for key, value := range formValues {
		formData.Set(key, value)
	}

	req, err := http.NewRequest("POST", urlStr, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://monitormywatershed.org")
	req.Header.Set("Referer", "https://monitormywatershed.org/tsv/")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

func (m *Monitor) fetchResultID() (map[string]string, error) {
	data := map[string]string{
		"request_data": fmt.Sprintf(`{"method":"get_sampling_feature_metadata","sampling_feature_code":"%s"}`, samplingCode),
	}
	response, err := m.makePostRequest(baseURL, data)
	if err != nil {
		return nil, err
	}

	var jsonStr string
	if err := json.Unmarshal(response, &jsonStr); err != nil {
		return nil, fmt.Errorf("first unmarshal error: %v", err)
	}

	var results []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		return nil, fmt.Errorf("second unmarshal error: %v", err)
	}

	measurements := make(map[string]string)
	for _, result := range results {
		name, ok1 := result["variablenamecv"].(string)
		resultID, ok2 := result["resultid"].(float64)
		if !ok1 || !ok2 {
			continue
		}
		measurements[name] = fmt.Sprintf("%d", int(resultID))
	}

	return measurements, nil
}

func (m *Monitor) fetchTimeSeriesData(name, resultID string) error {
	now := time.Now()
	startDate := now.Add(-5 * time.Minute).Format(time.RFC3339)
	endDate := now.Format(time.RFC3339)

	data := map[string]string{
		"request_data": fmt.Sprintf(`{"method":"get_result_timeseries","resultid":"%s","start_date":"%s","end_date":"%s"}`, resultID, startDate, endDate),
	}
	response, err := m.makePostRequest(baseURL, data)
	if err != nil {
		return fmt.Errorf("error fetching %s: %v", name, err)
	}

	var jsonStr string
	if err := json.Unmarshal(response, &jsonStr); err != nil {
		return fmt.Errorf("error parsing response for %s: %v", name, err)
	}

	var timeSeriesData TimeSeriesData
	if err := json.Unmarshal([]byte(jsonStr), &timeSeriesData); err != nil {
		return fmt.Errorf("error parsing data for %s: %v", name, err)
	}

	fmt.Printf("\n=== %s ===\n", name)
	for key := range timeSeriesData.DataValue {
		utcTime := time.Unix(timeSeriesData.ValueDateTime[key]/1000, 0)
		estTime := utcTime.Add(-5 * time.Hour)
		value := timeSeriesData.DataValue[key]

		fmt.Printf("Time (EST): %s\n", estTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("Value: %.2f\n", value)
		fmt.Println("-------------------")

		m.checkAndNotify(name, value, utcTime)
	}

	return nil
}

func (m *Monitor) loadState() error {
	input := &s3.GetObjectInput{
		Bucket: aws.String(m.bucketName),
		Key:    aws.String("last_email_times.json"),
	}

	result, err := m.s3Client.GetObject(input)
	if err != nil {
		// If the file doesn't exist, that's okay - we'll start fresh
		if strings.Contains(err.Error(), "NoSuchKey") {
			fmt.Println("No existing state found, starting fresh")
			m.lastEmailSent = make(map[string]time.Time)
			return nil
		}
		return fmt.Errorf("error loading state from S3: %v", err)
	}
	defer result.Body.Close()

	// Reset the map before loading
	m.lastEmailSent = make(map[string]time.Time)

	decoder := json.NewDecoder(result.Body)
	if err := decoder.Decode(&m.lastEmailSent); err != nil {
		return fmt.Errorf("error decoding state: %v", err)
	}

	// Print current state for debugging
	fmt.Println("Loaded state from S3:")
	for k, v := range m.lastEmailSent {
		fmt.Printf("- %s: last email sent at %v\n", k, v)
	}

	return nil
}

func (m *Monitor) saveState() error {
	if len(m.lastEmailSent) == 0 {
		fmt.Println("No state to save")
		return nil
	}

	data, err := json.Marshal(m.lastEmailSent)
	if err != nil {
		return fmt.Errorf("error marshaling state: %v", err)
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(m.bucketName),
		Key:    aws.String("last_email_times.json"),
		Body:   bytes.NewReader(data),
	}

	_, err = m.s3Client.PutObject(input)
	if err != nil {
		return fmt.Errorf("error saving state to S3: %v", err)
	}

	fmt.Println("Successfully saved state to S3")
	return nil
}

func (m *Monitor) RunOnce() error {
	fmt.Println("Starting monitor run...")

	// Load state at start
	if err := m.loadState(); err != nil {
		fmt.Printf("Warning: Could not load state: %v\n", err)
	}

	fmt.Println("Fetching measurement IDs...")
	measurements, err := m.fetchResultID()
	if err != nil {
		return fmt.Errorf("error fetching measurements: %v", err)
	}

	desiredMeasurements := []string{
		"Water depth",
		"Temperature",
		"Electrical conductivity",
		"Turbidity",
		"Battery voltage",
		"Percent full scale",
		"Relative humidity",
	}

	fmt.Printf("Found %d measurements, processing...\n", len(measurements))

	successCount := 0
	for _, name := range desiredMeasurements {
		if resultID, ok := measurements[name]; ok {
			fmt.Printf("Processing %s...\n", name)
			if err := m.fetchTimeSeriesData(name, resultID); err != nil {
				fmt.Printf("Error processing %s: %v\n", name, err)
				continue
			}
			successCount++
		}
	}

	fmt.Printf("Successfully processed %d/%d measurements\n", successCount, len(desiredMeasurements))

	// Save state after processing
	if err := m.saveState(); err != nil {
		fmt.Printf("Warning: Could not save state: %v\n", err)
	}

	fmt.Println("Monitor run completed")
	return nil
}
