package ratelimit

// Limiter is the interface for per-device rate limiting.
// Implementations: MemoryLimiter (standalone) and RedisLimiter (cluster/k8s).
//
// Every method takes the tenant that OWNS the device, not the tenant of
// whoever is asking (MESHSAT-1118). The budget, the counters and the admin
// override are all per (tenant, device): two tenants' devices must not share a
// send budget, and one tenant must not be able to spend or exempt another's.
type Limiter interface {
	// Allow checks if a send is permitted for the given device.
	// isSOS=true always returns true (emergency bypass).
	Allow(tenantID, deviceID string, isSOS bool) bool

	// Usage returns current rate limit status for a device.
	Usage(tenantID, deviceID string) DeviceUsage

	// AllUsage returns usage for the tenant's own tracked devices.
	AllUsage(tenantID string) []DeviceUsage
}
