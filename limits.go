package relay

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Community limits (docs/tenancy.md). Every one is off (0) by default.
type communityLimits struct {
	roomMaxBytes      int64 // LANGSTROTH_RELAY_ROOM_MAX_BYTES
	roomMaxEnvelopes  int64 // LANGSTROTH_RELAY_ROOM_MAX_ENVELOPES
	totalMaxBytes     int64 // LANGSTROTH_RELAY_MAX_TOTAL_ROOM_BYTES
	maxRoomsPerClient int64 // LANGSTROTH_RELAY_MAX_ROOMS_PER_CLIENT
	roomRetention     time.Duration

	ip   *ipRateLimiter // LANGSTROTH_RELAY_IP_RATE / _BURST: every request per client
	room *ipRateLimiter // LANGSTROTH_RELAY_ROOM_RATE / _BURST: every request per room
}

func communityLimitsFromEnv() communityLimits {
	pos := func(name string) int64 { return max(envInt64(name, 0), 0) }
	l := communityLimits{
		roomMaxBytes:      pos("LANGSTROTH_RELAY_ROOM_MAX_BYTES"),
		roomMaxEnvelopes:  pos("LANGSTROTH_RELAY_ROOM_MAX_ENVELOPES"),
		totalMaxBytes:     pos("LANGSTROTH_RELAY_MAX_TOTAL_ROOM_BYTES"),
		maxRoomsPerClient: pos("LANGSTROTH_RELAY_MAX_ROOMS_PER_CLIENT"),
		roomRetention:     daysEnv("LANGSTROTH_RELAY_ROOM_RETENTION_DAYS", 0),
	}
	if r := envFloat("LANGSTROTH_RELAY_IP_RATE", 0); r > 0 {
		l.ip = newIPRateLimiter(r, max(envFloat("LANGSTROTH_RELAY_IP_BURST", r*2), 1))
	}
	if r := envFloat("LANGSTROTH_RELAY_ROOM_RATE", 0); r > 0 {
		l.room = newIPRateLimiter(r, max(envFloat("LANGSTROTH_RELAY_ROOM_BURST", r*2), 1))
	}
	return l
}

// tooMany writes a 429 for limit with Retry-After (whole seconds, at least 1)
// and counts it.
func tooMany(w http.ResponseWriter, limit, tenant string, wait time.Duration, msg string) {
	countLimit(limit, tenant)
	secs := int64(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	http.Error(w, msg, http.StatusTooManyRequests)
}

// limitedPath reports whether the per-client request rate applies to path:
// everything but the status page, health and metrics.
func limitedPath(path string) bool {
	return path != "/" && path != "/v1/health" && path != "/metrics"
}

// quotaFor builds the write quotas for a request (relay-wide limits, then
// the caller's tenant's).
func (s *Server) quotaFor(r *http.Request) *writeQuota {
	ctx := r.Context()
	q := &writeQuota{
		Creator:          roomCreatorTag(writerTag(ctx, clientKey(r))),
		RoomMaxBytes:     s.lim.roomMaxBytes,
		RoomMaxEnvelopes: s.lim.roomMaxEnvelopes,
		TotalMaxBytes:    s.lim.totalMaxBytes,
		CreatorMaxRooms:  s.lim.maxRoomsPerClient,
	}
	if t := tenantFrom(ctx); t != nil {
		q.Tenant = t.id
		q.TenantMaxBytes = t.limits.MaxBytes
		q.TenantMaxRooms = t.limits.MaxRooms
		if t.limits.RoomMaxBytes > 0 {
			q.RoomMaxBytes = t.limits.RoomMaxBytes
		}
		if t.limits.RoomMaxEnvelopes > 0 {
			q.RoomMaxEnvelopes = t.limits.RoomMaxEnvelopes
		}
	}
	return q
}

// roomRetentionLoop expires idle rooms hourly (LANGSTROTH_RELAY_ROOM_RETENTION_DAYS).
func roomRetentionLoop(rs roomStore, idle time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			runPrune("idle_rooms", func() (int64, error) {
				return rs.PruneIdleRooms(context.Background(), time.Now().Add(-idle).Unix())
			})
		}
	}
}

// logLimits says at startup which community limits are on.
func (l communityLimits) log() {
	if l.roomMaxBytes == 0 && l.roomMaxEnvelopes == 0 && l.totalMaxBytes == 0 && l.maxRoomsPerClient == 0 &&
		l.roomRetention == 0 && l.ip == nil && l.room == nil {
		return
	}
	attrs := []any{
		"room_max_bytes", l.roomMaxBytes, "room_max_envelopes", l.roomMaxEnvelopes,
		"total_max_bytes", l.totalMaxBytes, "max_rooms_per_client", l.maxRoomsPerClient,
		"room_retention_days", int(l.roomRetention / (24 * time.Hour)),
	}
	if l.ip != nil {
		attrs = append(attrs, "ip_rate", l.ip.rate, "ip_burst", l.ip.burst)
	}
	if l.room != nil {
		attrs = append(attrs, "room_rate", l.room.rate, "room_burst", l.room.burst)
	}
	slog.Info("community limits", attrs...)
}
