//go:build android

package skirk

// Memory limits for Android environments.
//
// Android apps typically receive a 200–500 MB heap budget before being killed
// by the platform's low-memory killer. The values below are deliberately
// conservative so that even worst-case buffering (e.g. 4 active lanes plus
// global caps) stays well under 150 MB:
//
//	~4 × 8 MiB (lanes) + 48 MiB (global recv) + 48 MiB (global pending)
//	≈ 128 MiB of mux buffers.
//
// This file is selected automatically by the Go toolchain when GOOS=android
// (or when the `android` build tag is set), so no runtime check is needed.
const (
	muxNormalLaneQueueBytes     = 8 * 1024 * 1024  // 8 MiB per lane (vs 64 MiB on desktop)
	muxNormalStreamQueueBytes   = 2 * 1024 * 1024  // 2 MiB per stream
	muxNormalReceiveQueueBytes  = 8 * 1024 * 1024  // 8 MiB receive queue
	muxNormalReceiveGlobalBytes = 48 * 1024 * 1024 // 48 MiB global receive cap
	muxPendingStreamBytes       = 8 * 1024 * 1024  // 8 MiB per pending stream
	muxPendingGlobalBytes       = 48 * 1024 * 1024 // 48 MiB global pending cap
	muxStreamPendingBytes       = 8 * 1024 * 1024  // 8 MiB per stream pending
	// muxStreamPauseBytes must be strictly less than muxNormalLaneQueueBytes,
	// otherwise streams pause immediately on Android and never send data.
	muxStreamPauseBytes = 1 * 1024 * 1024 // 1 MiB pause threshold
)

// muxLimitsProfile is logged once per process at tunnel start so the chosen
// build profile (desktop vs android) is visible in field debug captures.
const muxLimitsProfile = "android"
