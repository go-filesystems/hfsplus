package hfsplus

import (
	"encoding/binary"
	"errors"
	iofs "io/fs"
	"os"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

func bsdOf(t *testing.T, v *Volume, p string) (uid, gid uint32, mode uint16) {
	t.Helper()
	parent, name, err := v.resolveParent(p)
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := v.recordBody(parent, name)
	if err != nil || !ok {
		t.Fatalf("recordBody(%s): %v %v", p, ok, err)
	}
	return binary.BigEndian.Uint32(body[32:]), binary.BigEndian.Uint32(body[36:]), binary.BigEndian.Uint16(body[42:])
}

// TestMetadataSetter: the volume satisfies the whole interface (the assertion
// smb's SET_INFO path, go-fileshare's lock wrapper, NFS and SFTP make).
func TestMetadataSetter(t *testing.T) {
	v, _ := formatChtimes(t)
	defer v.Close()
	var f filesystem.Filesystem = v
	ms, ok := f.(filesystem.MetadataSetter)
	if !ok {
		t.Fatal("hfsplus is not a filesystem.MetadataSetter")
	}
	now := time.Now()
	if err := ms.Chtimes("/a.txt", now, now); err != nil {
		t.Fatalf("Chtimes through the interface: %v", err)
	}
}

// TestChmodChown: the bits really change in the catalog record, for a file and
// a folder; the file-type bits and the other fields are kept; ctime follows the
// clock; Stat reports the new mode; a reopen sees it.
func TestChmodChown(t *testing.T) {
	now := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)
	pinNow(t, now)
	v, path := formatChtimes(t)
	for _, p := range []string{"/a.txt", "/sub", "/sub/c.txt"} {
		_, _, m0 := bsdOf(t, v, p)
		b0, mt0, _, a0 := timesAt(t, v, p)
		if err := v.Chmod(p, 0o640|os.ModeSetgid); err != nil {
			t.Fatalf("Chmod %s: %v", p, err)
		}
		if err := v.Chown(p, 501, 20); err != nil {
			t.Fatalf("Chown %s: %v", p, err)
		}
		uid, gid, m := bsdOf(t, v, p)
		if m&0o7777 != 0o2640 || m&^0o7777 != m0&^0o7777 {
			t.Errorf("%s: mode %#o (was %#o), want perm 02640 with the type bits kept", p, m, m0)
		}
		if uid != 501 || gid != 20 {
			t.Errorf("%s: owner %d:%d", p, uid, gid)
		}
		b, mt, c, a := timesAt(t, v, p)
		if !b.Equal(b0) || !mt.Equal(mt0) || !a.Equal(a0) || !c.Equal(now) {
			t.Errorf("%s: dates b=%v m=%v c=%v a=%v changed beyond ctime", p, b, mt, c, a)
		}
		st, err := v.Stat(p)
		if err != nil || uint16(st.Mode())&0o7777 != 0o2640 {
			t.Errorf("%s: Stat = %v, %v", p, st, err)
		}
	}
	// A chmod to 0 permission bits is stored as such (the type bits keep the
	// mode non-zero, so Stat does not fall back to the default).
	if err := v.Chmod("/b.txt", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, m := bsdOf(t, v, "/b.txt"); m&0o7777 != 0 {
		t.Errorf("mode %#o", m)
	}
	v.Close()
	re, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if uid, gid, m := bsdOf(t, re, "/sub/c.txt"); uid != 501 || gid != 20 || m&0o7777 != 0o2640 {
		t.Errorf("after reopen: %d:%d %#o", uid, gid, m)
	}
	if got, err := re.ReadFile("/a.txt"); err != nil || string(got) != "x" {
		t.Errorf("content: %q %v", got, err)
	}
}

func TestChmodChownRefusals(t *testing.T) {
	v, path := formatChtimes(t)
	defer v.Close()
	for name, err := range map[string]error{
		"Chmod missing": v.Chmod("/nope", 0o600),
		"Chown missing": v.Chown("/nope", 1, 1),
		"Chmod parent":  v.Chmod("/nope/x", 0o600),
		"Chmod through": v.Chmod("/a.txt/x", 0o600),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := v.Chmod("/nope", 0o600); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("Chmod missing = %v, want ErrNotExist", err)
	}
	if err := v.Chmod("/", 0o700); err == nil {
		t.Error("the root is refused")
	}
	if err := v.Chown("/", 1, 1); err == nil {
		t.Error("the root is refused")
	}
	pinNow(t, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	var re *TimeRangeError
	if err := v.Chmod("/a.txt", 0o600); !errors.As(err, &re) {
		t.Errorf("clock out of range: %v", err)
	}
	if _, _, m := bsdOf(t, v, "/a.txt"); m&0o7777 != 0o644 {
		t.Errorf("a refused Chmod changed the mode to %#o", m)
	}
	v.Close()
	ro, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.Chmod("/a.txt", 0o600); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Chmod read-only = %v", err)
	}
	if err := ro.Chown("/a.txt", 1, 1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Chown read-only = %v", err)
	}
}

// TestChmodManyRecords patches records across several leaf nodes and checks
// the tree still lists and reads them all.
func TestChmodManyRecords(t *testing.T) {
	v, _ := formatChtimes(t)
	defer v.Close()
	for i := 0; i < 200; i++ {
		p := "/m" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		if err := v.WriteFile(p, []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := v.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := v.Chown(p, uint32(i), uint32(i)); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := v.ListDir("/")
	if err != nil || len(ents) != 203 {
		t.Fatalf("ListDir: %d %v", len(ents), err)
	}
}
