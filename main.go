package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus-community/pro-bing"
)

type Config struct {
	Domain          string
	BotToken        string
	ChatID          string
	Threshold       int
	PingTimeout     time.Duration
	CheckInterval   time.Duration
	MessagePowered  string
	MessageBlackout string
	LabelDay        string
	LabelHour       string
	LabelMinute     string
	LabelSecond     string
	StateFilePath   string
}

type State struct {
	IsPowered bool
	Since     time.Time
}

type Monitor struct {
	config  Config
	history []bool
	state   *State
	client  *http.Client
}

func LoadConfig() (Config, error) {
	threshold, err := strconv.Atoi(getEnvOrDefault("THRESHOLD", "2"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid THRESHOLD: %w", err)
	}

	pingTimeout, err := strconv.Atoi(getEnvOrDefault("PING_TIMEOUT", "5"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid PING_TIMEOUT: %w", err)
	}

	checkInterval, err := strconv.Atoi(getEnvOrDefault("CHECK_INTERVAL", "15"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid CHECK_INTERVAL: %w", err)
	}

	if pingTimeout > checkInterval {
		return Config{}, fmt.Errorf("invalid PING_TIMEOUT, can't be longer then CHECK_INTERVAL")
	}

	domain := os.Getenv("DOMAIN")
	if domain == "" {
		return Config{}, fmt.Errorf("DOMAIN environment variable is required")
	}

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		return Config{}, fmt.Errorf("BOT_TOKEN environment variable is required")
	}

	chatID := os.Getenv("CHAT_ID")
	if chatID == "" {
		return Config{}, fmt.Errorf("CHAT_ID environment variable is required")
	}

	return Config{
		Domain:          domain,
		BotToken:        botToken,
		ChatID:          chatID,
		Threshold:       threshold,
		PingTimeout:     time.Duration(pingTimeout) * time.Second,
		CheckInterval:   time.Duration(checkInterval) * time.Second,
		MessagePowered:  getEnvOrDefault("MESSAGE_POWERED", "✅ Блекаут скінчився!"),
		MessageBlackout: getEnvOrDefault("MESSAGE_BLACKOUT", "⚡ Відключення електроенергії"),
		LabelDay:        getEnvOrDefault("LABEL_DAY", "дн"),
		LabelHour:       getEnvOrDefault("LABEL_HOUR", "год"),
		LabelMinute:     getEnvOrDefault("LABEL_MINUTE", "хв"),
		LabelSecond:     getEnvOrDefault("LABEL_SECOND", "сек"),
		StateFilePath:   getEnvOrDefault("STATE_FILE_PATH", "/var/lib/electricity-monitor/state.json"),
	}, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func formatDuration(d time.Duration, config Config) string {
	d = d.Round(time.Second)

	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour

	hours := d / time.Hour
	d -= hours * time.Hour

	minutes := d / time.Minute
	d -= minutes * time.Minute

	seconds := d / time.Second

	var parts []string

	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d%s", days, config.LabelDay))
	}

	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d%s", hours, config.LabelHour))
	}

	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d%s", minutes, config.LabelMinute))
	}

	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d%s", seconds, config.LabelSecond))
	}

	if len(parts) == 0 {
		return fmt.Sprintf("0 %s", config.LabelSecond)
	}

	if len(parts) == 1 {
		return parts[0]
	}

	return joinWithSpaces(parts)
	// return joinWithComma(parts)

	// if len(parts) == 2 {
	// 	return parts[0] + " and " + parts[1]
	// }

	// lastPart := parts[len(parts)-1]
	// firstParts := parts[:len(parts)-1]

	// return fmt.Sprintf("%s, and %s", joinWithComma(firstParts), lastPart)
}

func joinWithComma(parts []string) string {
	result := ""
	for i, part := range parts {
		if i > 0 {
			result += ", "
		}
		result += part
	}
	return result
}

func joinWithSpaces(parts []string) string {
	result := ""
	for i, part := range parts {
		if i > 0 {
			result += " "
		}
		result += part
	}
	return result
}

func NewMonitor(config Config) *Monitor {
	return &Monitor{
		config:  config,
		history: make([]bool, 0, config.Threshold),
		state:   nil,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (m *Monitor) saveState() error {
	if m.state == nil {
		return nil
	}

	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(m.config.StateFilePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	log.Printf("State saved to %s", m.config.StateFilePath)
	return nil
}

func (m *Monitor) loadState() error {
	data, err := os.ReadFile(m.config.StateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No existing state file found at %s, starting fresh", m.config.StateFilePath)
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("failed to unmarshal state: %w", err)
	}

	m.state = &state
	log.Printf("State loaded from %s: powered=%v, since=%s",
		m.config.StateFilePath, state.IsPowered, state.Since.Format(time.RFC3339))
	return nil
}

func (m *Monitor) ping() (bool, error) {
	pinger, err := probing.NewPinger(m.config.Domain)
	if err != nil {
		return false, fmt.Errorf("failed to create pinger: %w", err)
	}

	pinger.Count = 1
	pinger.Timeout = m.config.PingTimeout
	pinger.SetPrivileged(true)

	err = pinger.Run()
	if err != nil {
		return false, fmt.Errorf("ping failed: %w", err)
	}

	stats := pinger.Statistics()
	return stats.PacketsRecv > 0, nil
}

func (m *Monitor) sendTelegramMessage(message string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.config.BotToken)

	payload := map[string]string{
		"chat_id": m.config.ChatID,
		"text":    message,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	resp, err := m.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d", resp.StatusCode)
	}

	return nil
}

func (m *Monitor) updateState(isAlive bool) {
	m.history = append(m.history, isAlive)

	if len(m.history) > m.config.Threshold {
		m.history = m.history[1:]
	}

	if len(m.history) < m.config.Threshold {
		return
	}

	allSame := true
	firstValue := m.history[0]
	for _, v := range m.history {
		if v != firstValue {
			allSame = false
			break
		}
	}

	if !allSame {
		return
	}

	currentState := firstValue
	now := time.Now()

	if m.state == nil {
		m.state = &State{
			IsPowered: currentState,
			Since:     now,
		}
		message := m.config.MessageBlackout
		if currentState {
			message = m.config.MessagePowered
		}
		log.Printf("Initial state established: powered=%v at %s", currentState, now.Format(time.RFC3339))
		if err := m.saveState(); err != nil {
			log.Printf("Failed to save state: %v", err)
		}
		if err := m.sendTelegramMessage(message); err != nil {
			log.Printf("Failed to send initial state message: %v", err)
		}
		return
	}

	if m.state.IsPowered != currentState {
		duration := now.Sub(m.state.Since)
		oldState := m.state.IsPowered

		var messageTitle string
		if currentState {
			messageTitle = m.config.MessagePowered
		} else {
			messageTitle = m.config.MessageBlackout
		}
		message := fmt.Sprintf("%s: %s", messageTitle, formatDuration(duration, m.config))
		// message := fmt.Sprintf("%s\n\n%s", messageTitle, durationFormatted)

		log.Printf("State changed: powered=%v -> powered=%v, duration: %s", oldState, currentState, formatDuration(duration, m.config))

		m.state = &State{
			IsPowered: currentState,
			Since:     now,
		}

		if err := m.saveState(); err != nil {
			log.Printf("Failed to save state: %v", err)
		}

		if err := m.sendTelegramMessage(message); err != nil {
			log.Printf("Failed to send state change message: %v", err)
		}
	}
}

func (m *Monitor) Run(ctx context.Context) error {
	// Load existing state from disk
	if err := m.loadState(); err != nil {
		log.Printf("Warning: Failed to load state: %v", err)
	}

	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	log.Printf("Starting electricity monitor for %s (threshold: %d, interval: %s)",
		m.config.Domain, m.config.Threshold, m.config.CheckInterval)

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down monitor...")
			return ctx.Err()
		case <-ticker.C:
			isAlive, err := m.ping()
			if err != nil {
				log.Printf("Ping error: %v (treating as down)", err)
				isAlive = false
			}

			log.Printf("Ping result: powered=%v", isAlive)
			m.updateState(isAlive)
		}
	}
}

func main() {
	config, err := LoadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	monitor := NewMonitor(config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		cancel()
	}()

	if err := monitor.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("Monitor error: %v", err)
	}

	log.Println("Monitor stopped")
}
