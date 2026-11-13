//go:build !linux

package desktop

func ProcStatus(_, _ string) uint64 { return 0 }

func ResetPeak(_ string) {}

func ProcRSS(_ int) (RSS, bool) { return RSS{}, false }

func selfPss() (RSS, uint64, bool) { return RSS{}, 0, false }
