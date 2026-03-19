// meshsat-sim is a MeshSat device simulator that generates synthetic satellite
// MO messages and POSTs them to the Hub's webhook endpoints.
//
// Usage:
//
//	meshsat-sim --hub-url http://localhost:6070 --devices 3 --interval 30s
//	meshsat-sim --channel astrocast --pattern route --devices 2
//
// Each simulated device generates:
//   - Position reports with configurable movement patterns
//   - Text messages (random from a pool of realistic messages)
//   - Occasional SOS events (1 in 50 messages)
//   - Responds to MT messages via MQTT (simulated delivery confirmation)
//
// Movement patterns:
//   - random: random walk around center point (default)
//   - route: follows a predefined circular route
//   - stationary: fixed position, no movement
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var (
	hubURL   = flag.String("hub-url", envOr("SIM_HUB_URL", "http://localhost:6070"), "Hub base URL")
	devices  = flag.Int("devices", envInt("SIM_DEVICES", 3), "Number of simulated devices")
	interval = flag.Duration("interval", envDur("SIM_INTERVAL", 30*time.Second), "Message interval per device")
	secret   = flag.String("secret", envOr("SIM_WEBHOOK_SECRET", ""), "Webhook shared secret (RockBLOCK JWT or Astrocast HMAC)")
	channel  = flag.String("channel", envOr("SIM_CHANNEL", "iridium"), "Channel: iridium or astrocast")
	pattern  = flag.String("pattern", envOr("SIM_PATTERN", "random"), "Movement pattern: random, route, or stationary")
	mqttURL  = flag.String("mqtt-url", envOr("SIM_MQTT_URL", "tcp://localhost:6071"), "MQTT broker URL for MT subscription")

	centerLat = flag.Float64("lat", 52.3676, "Center latitude")
	centerLon = flag.Float64("lon", 4.9041, "Center longitude")
)

// Simulated IMEI pool — realistic 15-digit Iridium IMEIs.
var imeiPool = []string{
	"300234063904190", "300234063904191", "300234063904192",
	"300234063904193", "300234063904194", "300234063904195",
	"300234063904196", "300234063904197", "300234063904198",
	"300234063904199",
}

// Realistic message pool.
var messagePool = []string{
	"All clear at checkpoint alpha",
	"Moving to rally point bravo",
	"Weather deteriorating, high winds from NW",
	"Water resupply needed at base camp",
	"Team 2 in position, standing by",
	"Trail blocked by fallen tree, rerouting",
	"Reached summit, visibility 20km",
	"Camp established, all personnel accounted for",
	"Equipment check complete, ready to proceed",
	"Returning to base, ETA 2 hours",
	"Signal quality good, 4 bars",
	"Battery at 65%, solar charging active",
	"Wildlife spotted: bear, maintaining distance",
	"River crossing successful, all gear dry",
	"Night camp setup complete, fire watch posted",
}

type simulatedDevice struct {
	mu      sync.Mutex
	imei    string
	lat     float64
	lon     float64
	momsn   int
	step    int // route step counter
	pattern string
}

// updatePosition moves the device according to its movement pattern.
func (d *simulatedDevice) updatePosition() {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch d.pattern {
	case "stationary":
		// No movement.
	case "route":
		// Circular route: ~5km radius around center, one full loop per 360 steps.
		d.step++
		angle := float64(d.step) * (2 * math.Pi / 360)
		radius := 0.045 // ~5km in degrees
		d.lat = *centerLat + radius*math.Sin(angle)
		d.lon = *centerLon + radius*math.Cos(angle)
	default: // "random"
		d.lat += (rand.Float64() - 0.5) * 0.005
		d.lon += (rand.Float64() - 0.5) * 0.005
	}
}

func (d *simulatedDevice) pos() (float64, float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lat, d.lon
}

func (d *simulatedDevice) nextMOSN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.momsn++
	return d.momsn
}

func main() {
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("meshsat-sim starting",
		"hub", *hubURL,
		"devices", *devices,
		"interval", *interval,
		"channel", *channel,
		"pattern", *pattern,
		"mqtt", *mqttURL,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Create simulated devices.
	devs := make([]*simulatedDevice, *devices)
	for i := range devs {
		imei := imeiPool[i%len(imeiPool)]
		devs[i] = &simulatedDevice{
			imei:    imei,
			lat:     *centerLat + (rand.Float64()-0.5)*0.1,
			lon:     *centerLon + (rand.Float64()-0.5)*0.1,
			momsn:   1,
			pattern: *pattern,
		}
		slog.Info("sim: device created",
			"imei", imei,
			"lat", devs[i].lat,
			"lon", devs[i].lon,
			"pattern", *pattern,
		)
	}

	// Connect to MQTT for MT message subscription.
	mqttClient := connectMQTT(ctx, devs)

	// Start message generation loop.
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Send first batch immediately.
	for _, dev := range devs {
		go sendMessage(httpClient, dev)
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("meshsat-sim stopping")
			if mqttClient.IsConnected() {
				mqttClient.Disconnect(1000)
			}
			slog.Info("meshsat-sim stopped")
			return
		case <-ticker.C:
			for _, dev := range devs {
				go sendMessage(httpClient, dev)
			}
		}
	}
}

// connectMQTT subscribes to MT send topics for all devices and publishes
// simulated delivery confirmations.
func connectMQTT(ctx context.Context, devs []*simulatedDevice) mqtt.Client {
	opts := mqtt.NewClientOptions().
		AddBroker(*mqttURL).
		SetClientID(fmt.Sprintf("meshsat-sim-%d", os.Getpid())).
		SetAutoReconnect(true).
		SetCleanSession(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			slog.Info("sim: MQTT connected", "broker", *mqttURL)
			// Subscribe to MT send topics for all devices.
			for _, dev := range devs {
				topic := fmt.Sprintf("meshsat/%s/mt/send", dev.imei)
				token := c.Subscribe(topic, 1, makeMTHandler(c, dev))
				token.Wait()
				if token.Error() != nil {
					slog.Error("sim: MQTT subscribe failed", "topic", topic, "error", token.Error())
				} else {
					slog.Info("sim: subscribed to MT", "topic", topic)
				}
			}
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("sim: MQTT connection lost", "error", err)
		})

	client := mqtt.NewClient(opts)
	token := client.Connect()

	// Non-blocking: don't fail if MQTT isn't available yet.
	go func() {
		if token.WaitTimeout(10*time.Second) && token.Error() != nil {
			slog.Warn("sim: MQTT connect failed (MT delivery simulation disabled)", "error", token.Error())
		}
	}()

	return client
}

// makeMTHandler returns an MQTT message handler that simulates MT delivery
// for a specific device. On receiving an MT send request, it waits a realistic
// delay (5-15s simulating satellite pass) and publishes a delivery confirmation.
func makeMTHandler(c mqtt.Client, dev *simulatedDevice) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var req struct {
			Text     string `json:"text"`
			Priority int    `json:"priority"`
		}
		if err := json.Unmarshal(msg.Payload(), &req); err != nil {
			slog.Warn("sim: invalid MT request", "device", dev.imei, "error", err)
			return
		}

		slog.Info("sim: MT received",
			"device", dev.imei,
			"text", truncate(req.Text, 40),
			"priority", req.Priority,
		)

		// Simulate satellite delivery delay.
		go func() {
			delay := time.Duration(5+rand.IntN(10)) * time.Second
			time.Sleep(delay)

			// 95% delivery success rate.
			status := "delivered"
			errMsg := ""
			if rand.IntN(20) == 0 {
				status = "failed"
				errMsg = "simulated: satellite pass timeout"
			}

			statusMsg := map[string]any{
				"id":        fmt.Sprintf("sim-mt-%d", time.Now().UnixNano()),
				"channel":   *channel,
				"status":    status,
				"error":     errMsg,
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			}

			payload, _ := json.Marshal(statusMsg)
			statusTopic := fmt.Sprintf("meshsat/%s/mt/status", dev.imei)
			token := c.Publish(statusTopic, 1, false, payload)
			token.Wait()

			slog.Info("sim: MT delivery",
				"device", dev.imei,
				"status", status,
				"delay", delay.Round(time.Second),
			)
		}()
	}
}

func sendMessage(client *http.Client, dev *simulatedDevice) {
	dev.updatePosition()
	momsn := dev.nextMOSN()

	// Pick message: 1 in 50 chance of SOS.
	text := messagePool[rand.IntN(len(messagePool))]
	if rand.IntN(50) == 0 {
		text = "SOS EMERGENCY need immediate assistance at current position"
	}

	lat, lon := dev.pos()

	if *channel == "astrocast" {
		sendAstrocast(client, dev.imei, momsn, lat, lon, text)
	} else {
		sendRockBLOCK(client, dev.imei, momsn, lat, lon, text)
	}
}

func sendRockBLOCK(client *http.Client, imei string, momsn int, lat, lon float64, text string) {
	transmitTime := time.Now().UTC().Format("06-01-02 15:04:05")
	form := url.Values{
		"imei":              {imei},
		"momsn":             {fmt.Sprintf("%d", momsn)},
		"transmit_time":     {transmitTime},
		"iridium_latitude":  {fmt.Sprintf("%.4f", lat)},
		"iridium_longitude": {fmt.Sprintf("%.4f", lon)},
		"iridium_cep":       {fmt.Sprintf("%d", 5+rand.IntN(20))},
		"data":              {hex.EncodeToString([]byte(text))},
	}
	if *secret != "" {
		form.Set("JWT", *secret)
	}

	webhookURL := *hubURL + "/api/webhook/rockblock"
	resp, err := client.PostForm(webhookURL, form)
	if err != nil {
		slog.Error("sim: POST failed", "imei", imei, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	slog.Info("sim: MO sent",
		"channel", "iridium",
		"imei", imei,
		"momsn", momsn,
		"lat", math.Round(lat*1e4)/1e4,
		"lon", math.Round(lon*1e4)/1e4,
		"text", truncate(text, 40),
		"status", resp.StatusCode,
	)
}

func sendAstrocast(client *http.Client, deviceGUID string, momsn int, lat, lon float64, text string) {
	recvDate := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"deviceGuid":   deviceGUID,
		"messageGuid":  fmt.Sprintf("sim-%d-%d", momsn, time.Now().UnixNano()),
		"data":         base64.StdEncoding.EncodeToString([]byte(text)),
		"receivedDate": recvDate,
		"latitude":     lat,
		"longitude":    lon,
	}

	body, _ := json.Marshal(payload)
	webhookURL := *hubURL + "/api/webhook/astrocast"
	resp, err := client.Post(webhookURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		slog.Error("sim: POST failed", "device", deviceGUID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	slog.Info("sim: MO sent",
		"channel", "astrocast",
		"device", deviceGUID,
		"momsn", momsn,
		"lat", math.Round(lat*1e4)/1e4,
		"lon", math.Round(lon*1e4)/1e4,
		"text", truncate(text, 40),
		"status", resp.StatusCode,
	)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
		return n
	}
	return fallback
}

func envDur(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
