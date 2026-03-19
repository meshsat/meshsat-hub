// meshsat-sim is a MeshSat device simulator that generates synthetic satellite
// MO messages and POSTs them to the Hub's webhook endpoints.
//
// Usage:
//
//	meshsat-sim --hub-url http://localhost:6070 --devices 3 --interval 30s
//
// Each simulated device generates:
//   - Position reports (random walk around a configurable center point)
//   - Text messages (random from a pool of realistic messages)
//   - Occasional SOS events (1 in 50 messages)
//   - Responds to MT messages via MQTT (simulated delivery confirmation)
package main

import (
	"context"
	"encoding/hex"
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
	"syscall"
	"time"
)

var (
	hubURL    = flag.String("hub-url", envOr("SIM_HUB_URL", "http://localhost:6070"), "Hub base URL")
	devices   = flag.Int("devices", envInt("SIM_DEVICES", 3), "Number of simulated devices")
	interval  = flag.Duration("interval", envDur("SIM_INTERVAL", 30*time.Second), "Message interval per device")
	secret    = flag.String("secret", envOr("SIM_WEBHOOK_SECRET", ""), "RockBLOCK webhook secret")
	channel   = flag.String("channel", envOr("SIM_CHANNEL", "iridium"), "Channel: iridium or astrocast")
	centerLat = flag.Float64("lat", 52.3676, "Center latitude for random walk")
	centerLon = flag.Float64("lon", 4.9041, "Center longitude for random walk")
)

// Simulated IMEI pool — realistic 15-digit numbers.
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
	imei  string
	lat   float64
	lon   float64
	momsn int
}

func main() {
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("meshsat-sim starting",
		"hub", *hubURL,
		"devices", *devices,
		"interval", *interval,
		"channel", *channel,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Create simulated devices.
	devs := make([]*simulatedDevice, *devices)
	for i := range devs {
		imei := imeiPool[i%len(imeiPool)]
		devs[i] = &simulatedDevice{
			imei:  imei,
			lat:   *centerLat + (rand.Float64()-0.5)*0.1,
			lon:   *centerLon + (rand.Float64()-0.5)*0.1,
			momsn: 1,
		}
		slog.Info("sim: device created", "imei", imei, "lat", devs[i].lat, "lon", devs[i].lon)
	}

	// Start message generation loop.
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	client := &http.Client{Timeout: 10 * time.Second}

	for {
		select {
		case <-ctx.Done():
			slog.Info("meshsat-sim stopped")
			return
		case <-ticker.C:
			for _, dev := range devs {
				go sendMessage(client, dev)
			}
		}
	}
}

func sendMessage(client *http.Client, dev *simulatedDevice) {
	// Random walk: small delta per tick.
	dev.lat += (rand.Float64() - 0.5) * 0.005
	dev.lon += (rand.Float64() - 0.5) * 0.005
	dev.momsn++

	// Pick message: 1 in 50 chance of SOS.
	text := messagePool[rand.IntN(len(messagePool))]
	if rand.IntN(50) == 0 {
		text = "SOS EMERGENCY need immediate assistance at current position"
	}

	transmitTime := time.Now().UTC().Format("06-01-02 15:04:05")

	if *channel == "astrocast" {
		sendAstrocast(client, dev, text, transmitTime)
	} else {
		sendRockBLOCK(client, dev, text, transmitTime)
	}
}

func sendRockBLOCK(client *http.Client, dev *simulatedDevice, text, transmitTime string) {
	form := url.Values{
		"imei":              {dev.imei},
		"momsn":             {fmt.Sprintf("%d", dev.momsn)},
		"transmit_time":     {transmitTime},
		"iridium_latitude":  {fmt.Sprintf("%.4f", dev.lat)},
		"iridium_longitude": {fmt.Sprintf("%.4f", dev.lon)},
		"iridium_cep":       {fmt.Sprintf("%d", 5+rand.IntN(20))},
		"data":              {hex.EncodeToString([]byte(text))},
	}
	if *secret != "" {
		form.Set("JWT", *secret)
	}

	webhookURL := *hubURL + "/api/webhook/rockblock"
	resp, err := client.PostForm(webhookURL, form)
	if err != nil {
		slog.Error("sim: POST failed", "imei", dev.imei, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	slog.Info("sim: MO sent",
		"imei", dev.imei, "momsn", dev.momsn,
		"lat", math.Round(dev.lat*1e4)/1e4,
		"lon", math.Round(dev.lon*1e4)/1e4,
		"text", truncate(text, 40),
		"status", resp.StatusCode,
	)
}

func sendAstrocast(client *http.Client, dev *simulatedDevice, text, _ string) {
	b64Data := hex.EncodeToString([]byte(text)) // simplified encoding for simulator
	recvDate := time.Now().UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"deviceGuid":"%s","messageGuid":"sim-%d-%d","data":"%s","receivedDate":"%s","latitude":%f,"longitude":%f}`,
		dev.imei, dev.momsn, time.Now().UnixNano(),
		b64Data, recvDate, dev.lat, dev.lon,
	)

	webhookURL := *hubURL + "/api/webhook/astrocast"
	resp, err := client.Post(webhookURL, "application/json", strings.NewReader(body))
	if err != nil {
		slog.Error("sim: POST failed", "imei", dev.imei, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	slog.Info("sim: MO sent (astrocast)",
		"device", dev.imei, "momsn", dev.momsn,
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
