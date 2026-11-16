package desktop

import "runtime"

type Memory struct {
	HeapAlloc  int64
	Sys        int64
	Goroutines int
	Total      int64
	Anon       int64
	// Not Sys: that counts mapped address space, larger than the resident Anon.
	GoInuse   int64
	GoIdle    int64
	Native    int64
	File      int64
	DB        int64
	OtherFile int64
	Available bool
}

type RSS struct {
	Total uint64
	Anon  uint64
	File  uint64
}

func ReadMemory() Memory {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// GCSys, OtherSys and BuckHashSys are left out: mapped far larger than
	// they are faulted in, they would push the Go share past RssAnon.
	goResident := int64(ms.HeapSys-ms.HeapReleased) + int64(ms.StackSys+ms.MSpanSys+ms.MCacheSys)
	out := Memory{
		HeapAlloc:  int64(ms.HeapAlloc),
		Sys:        int64(ms.Sys),
		GoInuse:    int64(ms.HeapInuse + ms.StackInuse + ms.MSpanInuse + ms.MCacheInuse),
		Goroutines: runtime.NumGoroutine(),
	}
	out.GoIdle = goResident - out.GoInuse
	if r, db, ok := selfPss(); ok {
		out.Total = int64(r.Total)
		out.Anon = int64(r.Anon)
		out.File = int64(r.File)
		out.DB = int64(db)
		out.OtherFile = max(out.File-out.DB, 0)
		// Clamped: the two readings are taken apart, and a swapped-out
		// page leaves one but not the other.
		out.Native = max(out.Anon-goResident, 0)
		out.Available = true
	}
	return out
}
