package desktop

import "testing"

func TestMemoryRowsNestTruthfully(t *testing.T) {
	m := ReadMemory()
	if m.GoInuse < m.HeapAlloc {
		t.Errorf("Go heap (live objects) %d is not inside Go runtime (in use) %d", m.HeapAlloc, m.GoInuse)
	}
	if !m.Available {
		t.Skip("no per-process residency source here; the table hides the rows below")
	}
	if goResident := m.GoInuse + m.GoIdle; goResident <= m.Anon && goResident+m.Native != m.Anon {
		t.Errorf("Go runtime (in use) %d + Go runtime (idle) %d + Native heap %d != Private (anonymous) %d",
			m.GoInuse, m.GoIdle, m.Native, m.Anon)
	}
	if m.DB+m.OtherFile != m.File {
		t.Errorf("DB pages %d + Code & shared libs %d != Shared (file-backed) %d", m.DB, m.OtherFile, m.File)
	}
}
