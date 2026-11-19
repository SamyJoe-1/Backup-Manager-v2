package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"backupmanager/internal/core"

	"github.com/klauspost/compress/zstd"
)

// catalogBackup writes a consistent copy of the catalog while the service runs.
func catalogBackup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: backup-manager catalog-backup /path/to/copy.db")
	}
	p := loadPaths()
	st, err := core.OpenStore(filepath.Join(p.data, "catalog.db"))
	if err != nil {
		return err
	}
	defer st.DB.Close()
	if err := core.SnapshotCatalog(st, args[0]); err != nil {
		return err
	}
	fmt.Println("catalog copied to", args[0])
	return nil
}

// decrypt opens one stored backup file with nothing but the master key: for when the server that
// ran Backup Manager is gone. meta.json sits in the same folder as the file (backup id/meta.json).
func decrypt(args []string) error {
	fs := flag.NewFlagSet("decrypt", flag.ContinueOnError)
	in := fs.String("in", "", "stored file, e.g. 20261115T…/shop/orders.data.sql.zst.enc")
	out := fs.String("out", "", "where to write the plain file (default: the name without .zst/.enc)")
	metaPath := fs.String("meta", "", "the backup's meta.json (default: meta.json next to the backup folder)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("usage: backup-manager decrypt --in FILE [--meta meta.json] [--out FILE]   (uses BM_MASTER_KEY_FILE)")
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(*in, ".enc") {
		mp := *metaPath
		if mp == "" {
			for d := filepath.Dir(*in); ; d = filepath.Dir(d) {
				if _, err := os.Stat(filepath.Join(d, "meta.json")); err == nil {
					mp = filepath.Join(d, "meta.json")
					break
				}
				if d == filepath.Dir(d) {
					return errors.New("meta.json not found above the file; pass --meta")
				}
			}
		}
		raw, err := os.ReadFile(mp)
		if err != nil {
			return err
		}
		var m core.Meta
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s: %w", mp, err)
		}
		wrapped, err := hex.DecodeString(m.WrappedKey)
		if err != nil || len(wrapped) == 0 {
			return errors.New("meta.json carries no key")
		}
		keys, _, err := core.LoadOrCreateMaster(loadPaths().key)
		if err != nil {
			return err
		}
		dk, err := keys.Open(wrapped)
		if err != nil {
			return errors.New("this master key does not open this backup (it was made with another key)")
		}
		if r, err = core.NewDecReader(r, dk); err != nil {
			return err
		}
	}
	z, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer z.Close()
	dst := *out
	if dst == "" {
		dst = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(*in), ".enc"), ".zst")
	}
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, z); err != nil {
		w.Close()
		os.Remove(dst)
		return fmt.Errorf("the file is damaged or incomplete: %w", err)
	}
	if err := w.Close(); err != nil {
		return err
	}
	fmt.Println("written", dst)
	return nil
}
