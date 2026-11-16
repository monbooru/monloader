//go:build linux

package desktop

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// ProcStatus reads one size field of /proc/<pid>/status, such as VmRSS or
// VmHWM, in bytes; pid is "self" or a process id.
func ProcStatus(pid, field string) uint64 {
	f, err := os.Open("/proc/" + pid + "/status")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(scanner.Text(), field+":"); ok {
			return parseKB(rest)
		}
	}
	return 0
}

// ResetPeak restarts the VmHWM that ProcStatus reads.
func ResetPeak(pid string) {
	_ = os.WriteFile("/proc/"+pid+"/clear_refs", []byte("5"), 0)
}

// Status, not smaps: the smaps walk costs time in proportion to what is
// resident, and a tagger worker holds gigabytes of unshared anonymous pages.
func ProcRSS(pid int) (RSS, bool) {
	return statusRSS("/proc/" + strconv.Itoa(pid))
}

func statusRSS(procDir string) (RSS, bool) {
	f, err := os.Open(procDir + "/status")
	if err != nil {
		return RSS{}, false
	}
	defer func() { _ = f.Close() }()
	var out RSS
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			out.Total = parseKB(line[len("VmRSS:"):])
		case strings.HasPrefix(line, "RssAnon:"):
			out.Anon = parseKB(line[len("RssAnon:"):])
		case strings.HasPrefix(line, "RssFile:"):
			out.File = parseKB(line[len("RssFile:"):])
		}
	}
	if out.Total == 0 {
		return RSS{}, false
	}
	return out, true
}

// Pss from smaps, not Rss: SQLite connections map the same database file,
// and Rss counts a shared page once per mapping. RssAnon is used as is,
// since anonymous pages are not shared.
func selfPss() (RSS, uint64, bool) {
	out, ok := statusRSS("/proc/self")
	if !ok {
		return RSS{}, 0, false
	}
	var db uint64
	if total, file, dbPss, ok := sumSmapsPss("/proc/self"); ok {
		out.Total, out.File, db = total, file, dbPss
	}
	return out, db, true
}

func sumSmapsPss(procDir string) (totalPss, filePss, dbPss uint64, ok bool) {
	f, err := os.Open(procDir + "/smaps")
	if err != nil {
		return 0, 0, 0, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var inFile, inDB bool
	for scanner.Scan() {
		line := scanner.Text()
		if isSmapsHeader(line) {
			path := smapsPath(line)
			inFile = path != "" && !strings.HasPrefix(path, "[")
			inDB = inFile && isDBPath(path)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Pss:"); ok {
			v := parseKB(rest)
			totalPss += v
			if inFile {
				filePss += v
				if inDB {
					dbPss += v
				}
			}
		}
	}
	return totalPss, filePss, dbPss, true
}

// Header lines start with a hex address; data lines with a capitalised key.
func isSmapsHeader(line string) bool {
	if line == "" {
		return false
	}
	c := line[0]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// Header: ADDR-ADDR perms offset dev inode path; the path may contain spaces.
func smapsPath(header string) string {
	fields := strings.Fields(header)
	if len(fields) < 6 {
		return ""
	}
	return strings.Join(fields[5:], " ")
}

func isDBPath(p string) bool {
	p = strings.TrimSuffix(strings.TrimSuffix(p, "-wal"), "-shm")
	return strings.HasSuffix(p, ".db") || strings.HasSuffix(p, ".sqlite") || strings.HasSuffix(p, ".sqlite3")
}

func parseKB(rest string) uint64 {
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}
