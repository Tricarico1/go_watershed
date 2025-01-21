package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
)

const (
	baseURL      = "https://monitormywatershed.org/dataloader/ajax/"
	samplingCode = "MSPL2S"
)

// Configuration that should come from AWS environment variables
var (
	gmailUser       = os.Getenv("GMAIL_USER")     // Will be set in AWS
	gmailPassword   = os.Getenv("GMAIL_PASSWORD") // Will be set in AWS
	smtpHost        = "smtp.gmail.com"
	smtpPort        = "587"
	emailRecipients = []string{os.Getenv("EMAIL_RECIPIENT")} // Will be set in AWS
)

// Thresholds matching your Python script
var thresholds = map[string]struct {
	max float64
	min float64
}{
	"Water depth":             {max: 1000, min: 0},
	"Temperature":             {max: 26, min: 0},
	"Electrical conductivity": {max: 600, min: 0},
	"Turbidity":               {max: 150, min: 0},
	"Battery voltage":         {max: 5, min: 0},
	"Percent full scale":      {max: 100, min: 0},
	"Relative humidity":       {max: 100, min: 0},
}

var lastEmailSent = make(map[string]time.Time)

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

func sendEmail(subject, body string) error {
	auth := smtp.PlainAuth("", gmailUser, gmailPassword, smtpHost)

	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s",
		gmailUser,
		emailRecipients[0],
		subject,
		body)

	return smtp.SendMail(
		smtpHost+":"+smtpPort,
		auth,
		gmailUser,
		emailRecipients,
		[]byte(msg),
	)
}

func sendEmailSES(subject, body string) error {
	// Create new AWS session
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String("us-east-1")}, // or your preferred region
	})
	if err != nil {
		return err
	}

	// Create SES service client
	svc := ses.New(sess)

	// Construct email
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

	_, err = svc.SendEmail(input)
	return err
}

func checkAndNotify(name string, value float64) {
	threshold, exists := thresholds[name]
	if !exists {
		return
	}

	// Always print to console
	if value >= threshold.max || value < threshold.min {
		fmt.Printf("\nALERT: %s value %.2f is outside acceptable range (%.2f to %.2f)\n",
			name, value, threshold.min, threshold.max)
	}

	// Only attempt email if configured
	if gmailUser == "" || gmailPassword == "" || len(emailRecipients) == 0 {
		return
	}

	lastSent, hasSent := lastEmailSent[name]
	now := time.Now()

	// Check if value is outside acceptable range (matching your Python logic)
	if (value >= threshold.max || value < threshold.min) &&
		(!hasSent || now.Sub(lastSent) >= 24*time.Hour) {

		subject := fmt.Sprintf("%s Alert", name)
		body := fmt.Sprintf("%s has reached %.2f (Acceptable range: %.2f to %.2f)",
			name, value, threshold.min, threshold.max)

		if err := sendEmail(subject, body); err != nil {
			fmt.Printf("Error sending email for %s: %v\n", name, err)
		} else {
			lastEmailSent[name] = now
			fmt.Printf("Alert email sent for %s (value: %.2f)\n", name, value)
		}
	}
}

func makePostRequest(urlStr string, formValues map[string]string) ([]byte, error) {
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

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func fetchResultID() (map[string]string, error) {
	data := map[string]string{
		"request_data": fmt.Sprintf(`{"method":"get_sampling_feature_metadata","sampling_feature_code":"%s"}`, samplingCode),
	}
	response, err := makePostRequest(baseURL, data)
	if err != nil {
		return nil, err
	}

	// First unmarshal the string
	var jsonStr string
	if err := json.Unmarshal(response, &jsonStr); err != nil {
		return nil, fmt.Errorf("first unmarshal error: %v", err)
	}

	// Then unmarshal the JSON array
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

func fetchTimeSeriesData(name, resultID string) {
	now := time.Now()
	startDate := now.Add(-5 * time.Minute).Format(time.RFC3339)
	endDate := now.Format(time.RFC3339)

	data := map[string]string{
		"request_data": fmt.Sprintf(`{"method":"get_result_timeseries","resultid":"%s","start_date":"%s","end_date":"%s"}`, resultID, startDate, endDate),
	}
	response, err := makePostRequest(baseURL, data)
	if err != nil {
		fmt.Printf("Error fetching %s: %v\n", name, err)
		return
	}

	var jsonStr string
	if err := json.Unmarshal(response, &jsonStr); err != nil {
		fmt.Printf("Error parsing response for %s: %v\n", name, err)
		return
	}

	var timeSeriesData TimeSeriesData
	if err := json.Unmarshal([]byte(jsonStr), &timeSeriesData); err != nil {
		fmt.Printf("Error parsing data for %s: %v\n", name, err)
		return
	}

	// Print the formatted data
	fmt.Printf("\n=== %s ===\n", name)
	for key := range timeSeriesData.DataValue {
		utcTime := time.Unix(timeSeriesData.ValueDateTime[key]/1000, 0)
		estTime := utcTime.Add(-5 * time.Hour)
		value := timeSeriesData.DataValue[key]

		fmt.Printf("Time (EST): %s\n", estTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("Value: %.2f\n", value)
		fmt.Println("-------------------")

		// Check if we need to send an alert
		checkAndNotify(name, value)
	}
}

func main() {
	emailConfigured := gmailUser != "" && gmailPassword != "" && len(emailRecipients) > 0
	if !emailConfigured {
		fmt.Println("Email not configured - running in monitoring-only mode")
	} else {
		fmt.Println("Email alerts configured and enabled")
	}

	fmt.Println("Starting monitoring service...")

	for {
		measurements, err := fetchResultID()
		if err != nil {
			fmt.Printf("Error fetching measurements: %v\n", err)
			time.Sleep(5 * time.Minute)
			continue
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

		for _, name := range desiredMeasurements {
			if resultID, ok := measurements[name]; ok {
				fetchTimeSeriesData(name, resultID)
			}
		}

		time.Sleep(5 * time.Minute)
	}
}
