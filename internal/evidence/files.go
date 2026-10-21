package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

type directory struct {
	root *os.Root
	path string
	info os.FileInfo
}

func directoryPath(db string) (string, error) {
	absolute, err := filepath.Abs(db)
	if err != nil {
		return "", err
	}
	// Match ownership locking when the database itself is a symlink.
	if resolved, e := filepath.EvalSymlinks(absolute); e == nil {
		absolute = resolved
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	return filepath.Join(filepath.Dir(absolute), "evidence"), nil
}
func openDirectory(db string, create bool) (*directory, error) {
	path, err := directoryPath(db)
	if err != nil {
		return nil, err
	}
	if create {
		if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode() != os.ModeDir|0700 {
		return nil, errors.New("evidence directory must be a real 0700 directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	d := &directory{root, path, info}
	if err = d.check(); err != nil {
		root.Close()
		return nil, err
	}
	return d, nil
}
func (d *directory) check() error {
	current, err := os.Lstat(d.path)
	if err != nil {
		return err
	}
	held, err := d.root.Stat(".")
	if err != nil {
		return err
	}
	if current.Mode() != os.ModeDir|0700 || !os.SameFile(current, d.info) || !os.SameFile(held, d.info) {
		return errors.New("evidence directory identity or permissions changed")
	}
	return nil
}
func names(id int64) (string, string) {
	return fmt.Sprintf("capture-%d.json", id), fmt.Sprintf(".capture-%d.tmp", id)
}
func (d *directory) sync() error {
	f, err := d.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (d *directory) read(name string, max int64) ([]byte, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode() != 0600 || info.Size() > max {
		return nil, errors.New("evidence file is unsafe or exceeds its allowance")
	}
	f, err := d.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, held) {
		return nil, errors.New("evidence file changed during open")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("evidence file grew beyond its allowance")
	}
	return b, nil
}
func (d *directory) remove(name string) error {
	if err := d.check(); err != nil {
		return err
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode() != 0600 {
		return errors.New("refusing to delete unsafe evidence file")
	}
	if err = d.root.Remove(name); err != nil {
		return err
	}
	return d.sync()
}
func (d *directory) usage() (int64, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	f, err := d.root.Open(".")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	entries, err := f.ReadDir(MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if len(entries) >= MaxEntries-1 {
		return 0, errors.New("evidence directory entry limit reached")
	}
	var total int64
	for _, entry := range entries {
		info, err := d.root.Lstat(entry.Name())
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, errors.New("unexpected non-regular evidence entry")
		}
		if info.Size() > 1024*1024*1024-total {
			return 0, errors.New("evidence storage exceeds the maximum allowance")
		}
		total += info.Size()
	}
	return total, nil
}
func (d *directory) publish(id int64, body []byte) (err error) {
	if err := d.check(); err != nil {
		return err
	}
	final, temp := names(id)
	f, err := d.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if err != nil && !published {
			err = errors.Join(err, d.remove(temp))
		}
	}()
	_, writeErr := f.Write(body)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = d.check(); err != nil {
		return err
	}
	// Hard-link publication is atomic and cannot replace an existing final name.
	if err = d.root.Link(temp, final); err != nil {
		return err
	}
	published = true
	if err = d.sync(); err != nil {
		return err
	}
	return d.remove(temp)
}
func digest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

// validatePublished checks both crash adoption and later integrity. It never
// trusts a path, a size, a status, or a capture identity supplied by the file.
func (d *directory) validatePublished(r store.CaptureRecord) (Bundle, []byte, error) {
	var p Limits
	var input Input
	var b Bundle
	if json.Unmarshal([]byte(r.Limits), &p) != nil || p.Validate() != nil || json.Unmarshal([]byte(r.Input), &input) != nil {
		return b, nil, errors.New("invalid evidence intent metadata")
	}
	final, _ := names(r.ActionID)
	if r.BundleName != "" && r.BundleName != final {
		return b, nil, errors.New("invalid evidence bundle name")
	}
	body, err := d.read(final, p.BundleBytes)
	if err != nil {
		return b, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&b); err != nil {
		return b, nil, errors.New("invalid evidence JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return b, nil, errors.New("trailing evidence JSON")
	}
	if b.SchemaVersion != 1 || !reflect.DeepEqual(b.Input, input) || input.ActionID != r.ActionID || !reflect.DeepEqual(b.Limits, p) || !b.StartedAt.Equal(r.StartedAt) || b.EndedAt.IsZero() || (b.Status != "complete" && b.Status != "partial") || len(b.Logs) > int(p.LogBytes) || len(b.Issues) > 24 {
		return b, nil, errors.New("published evidence does not match intent")
	}
	if b.Status == "complete" && (b.Before == nil || b.After == nil || b.Before.StartedAt.IsZero() || !b.Before.StartedAt.Equal(b.After.StartedAt)) {
		return b, nil, errors.New("complete evidence lacks stable execution identity")
	}
	if redact(b.Logs) != b.Logs {
		return b, nil, errors.New("evidence logs do not satisfy redaction rules")
	}
	if (b.Status == "complete" && (len(b.Issues) > 0 || b.Truncated)) || (b.Status == "partial" && len(b.Issues) == 0) {
		return b, nil, errors.New("inconsistent evidence completion status")
	}
	for _, issue := range b.Issues {
		if len(issue) > 128 {
			return b, nil, errors.New("invalid evidence issue")
		}
	}
	for _, c := range []*Container{b.Before, b.After} {
		if c != nil && c.ID != input.ContainerID {
			return b, nil, errors.New("evidence target changed")
		}
	}
	if r.Digest != "" && (r.Digest != digest(body) || r.Bytes != int64(len(body))) {
		return b, nil, errors.New("published evidence integrity check failed")
	}
	return b, body, nil
}
