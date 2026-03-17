package ratelimit

// Limiter is the interface for per-device rate limiting.
// Implementations: MemoryLimiter (standalone) and RedisLimiter (cluster/k8s).
type Limiter interface {
	// Allow checks if a send is permitted for the given device.
	// isSOS=true always returns true (emergency bypass).
	Allow(deviceID string, isSOS bool) bool

	// Usage returns current rate limit status for a device.
	Usage(deviceID string) DeviceUsage

	// AllUsage returns usage for all tracked devices.
	AllUsage() []DeviceUsage
}
