//go:build !android

package skirk

// Memory limits for desktop/server environments.
//
// These are generous values suitable for machines with 4 GB+ RAM. They favor
// throughput on bulk uploads/downloads by giving each lane a large in-memory
// window before back-pressuring the producer.
//
// For Android, see mux_limits_android.go which uses tighter caps tuned to a
// 200–500 MB heap budget to avoid the platform's low-memory killer.
const (
	muxNormalLaneQueueBytes     = 64 * 1024 * 1024  // 64 MiB per lane
	muxNormalStreamQueueBytes   = 16 * 1024 * 1024  // 16 MiB per stream
	muxNormalReceiveQueueBytes  = 64 * 1024 * 1024  // 64 MiB receive queue
	muxNormalReceiveGlobalBytes = 256 * 1024 * 1024 // 256 MiB global receive cap
	muxPendingStreamBytes       = 64 * 1024 * 1024  // 64 MiB per pending stream
	muxPendingGlobalBytes       = 256 * 1024 * 1024 // 256 MiB global pending cap
	muxStreamPendingBytes       = 64 * 1024 * 1024  // 64 MiB per stream pending
	// muxStreamPauseBytes must be < muxNormalLaneQueueBytes so streams do not
	// pause before they have a chance to send anything.
	muxStreamPauseBytes = 8 * 1024 * 1024 // 8 MiB pause threshold
)

// muxLimitsProfile is logged once per process at tunnel start so the chosen
// build profile (desktop vs android) is visible in field debug captures.
const muxLimitsProfile = "desktop"
