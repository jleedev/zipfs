package main

import (
	"archive/zip"
	"embed"
	"encoding/binary"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"mime"
	"net/http"
	"net/http/fcgi"
	"path"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"

	"blitiri.com.ar/go/systemd"
)

//go:embed template/*
var static embed.FS

var index *string = flag.String("index", "", "serve <index.html> if present")
var browse *bool = flag.Bool("browse", true, "serve directory listings")

var tmpl = template.Must(template.ParseFS(static, "template/*"))

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)
	slog.SetLogLoggerLevel(slog.LevelDebug)
	info, _ := debug.ReadBuildInfo()
	log.Printf("%#v", info.Main)

	http.Handle("GET /", NewZipServer())

	listeners, err := systemd.Listeners()
	if err != nil {
		log.Fatal("Socket activation failed: ", err)
	}
	for name, ls := range listeners {
		for _, l := range ls {
			slog.Info("Listening", "name", name, "network", l.Addr().Network(), "addr", l.Addr())
			log.Fatal(fcgi.Serve(l, nil))
		}
	}
	log.Fatal("No socket activation")
}

func NewZipServer() *ZipServer {
	return &ZipServer{
		archives: make(map[string]*zipFS),
		index:    *index,
		browse:   *browse,
	}
}

// Files are kept open forever. This will probably crash if a file is altered on disk. Working as intended.
type ZipServer struct {
	archives map[string]*zipFS
	rw       sync.RWMutex
	index    string
	browse   bool
}

func (z *ZipServer) getArchive(path string) (zf *zipFS, err error) {
	z.rw.RLock()
	zf = z.archives[path]
	z.rw.RUnlock()
	if zf != nil {
		return
	}
	var rc *zip.ReadCloser
	rc, err = zip.OpenReader(path)
	if err != nil {
		return
	}
	zf = newZipFS(rc)
	z.rw.Lock()
	z.archives[path] = zf
	z.rw.Unlock()
	return
}

func (z *ZipServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	env := fcgi.ProcessEnv(r)
	zf, err := z.getArchive(env["SCRIPT_FILENAME"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	orig_path := env["PATH_TRANSLATED"]
	p := strings.Trim(orig_path, "/")
	if p == "" {
		p = "."
	}

	entry, err := FindRaw(&zf.Reader, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer entry.Close()

	_, is_dir := entry.File.(fs.ReadDirFile)
	if strings.HasSuffix(orig_path, "/") && z.index != "" {
		if is_dir {
			// See if there's an index.html
			index_entry, err := FindRaw(&zf.Reader, path.Join(p, z.index))
			if err != nil {
				// Guess not
			} else {
				defer index_entry.Close()
				SendFile(zf, index_entry, w, r)
				return
			}
		}
	}

	if is_dir && !strings.HasSuffix(orig_path, "/") {
		// Canonicalize directory with a trailing slash
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}

	if strings.HasSuffix(orig_path, "/") && !is_dir {
		// We already handled index.html above, so this is an erroneous
		// trailing slash
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// At this point, both the request url and the zip entry agree
	if is_dir {
		SendDirectory(zf, entry, w, r)
	} else {
		SendFile(zf, entry, w, r)
	}
}

// Serves files from a single zip file
type zipFS struct {
	*zip.ReadCloser
	mimeCache map[*zip.File]string
	rw        sync.RWMutex
}

func newZipFS(z *zip.ReadCloser) *zipFS {
	return &zipFS{
		z,
		make(map[*zip.File]string),
		sync.RWMutex{},
	}
}

// Wrapper for the result of opening the path and then sneaking
// around to find the corresponding raw entry
// Entry may be nil if it's the root (or another nonexistent directory),
// but never if it's a file
type ZipEntry struct {
	fs.File
	Entry *zip.File
}

// Finds the named File entry in the ZIP archive
// Then do the dumb reflection work to pull out the underlying zip.File
// This is necessary because zip doesn't have OpenRaw(name string)
// and it's easier than processing the flat list of files myself
func FindRaw(z *zip.Reader, name string) (*ZipEntry, error) {
	f, err := z.Open(name)
	if err != nil {
		return nil, err
	}
	v := reflect.ValueOf(f).Elem()
	if v.FieldByName("e").IsValid() {
		v = v.FieldByName("e").Elem().FieldByName("file")
	} else {
		v = v.FieldByName("f")
	}
	entry := (*zip.File)(v.UnsafePointer())
	return &ZipEntry{f, entry}, nil
}

func SendDirectory(z *zipFS, entry *ZipEntry, w http.ResponseWriter, r *http.Request) {
	if entry.Entry != nil {
		w.Header().Set("Last-Modified", entry.Entry.Modified.Format(http.TimeFormat))
	}
	// Serve the directory listing
	rd := entry.File.(fs.ReadDirFile)
	entries, err := rd.ReadDir(-1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, "dir.html", struct {
		Path    string
		Entries []fs.DirEntry
	}{r.URL.Path, entries})
}

func SendFile(z *zipFS, entry *ZipEntry, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Last-Modified", entry.Entry.Modified.Format(http.TimeFormat))
	w.Header().Set("Content-Type", z.GetMime(entry.Entry))

	if !(entry.Entry.Method == zip.Deflate && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")) {
		// Just serve a plain response
		io.Copy(w, entry)
		return
	}

	// The entry is compressed and we're ready to serve up some gzip
	w.Header().Set("Content-Encoding", "gzip")

	fmt.Fprint(w, "\x1f\x8b\x08\x00")
	mtime := entry.Entry.Modified.Unix()
	binary.Write(w, binary.LittleEndian, uint32(mtime))
	fmt.Fprint(w, "\x00\xff")

	src, err := entry.Entry.OpenRaw()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.Copy(w, src)

	binary.Write(w, binary.LittleEndian, []uint32{
		entry.Entry.CRC32,
		uint32(entry.Entry.UncompressedSize64 % 0x1_0000_0000),
	})
}

func (z *zipFS) GetMime(f *zip.File) string {
	z.rw.RLock()
	if x, ok := z.mimeCache[f]; ok {
		z.rw.RUnlock()
		return x
	}
	z.rw.RUnlock()
	ctype := mime.TypeByExtension(filepath.Ext(f.Name))
	if ctype != "" {
		z.rw.Lock()
		z.mimeCache[f] = ctype
		z.rw.Unlock()
		return ctype
	}
	r, err := f.Open()
	if err != nil {
		panic(err)
	}
	defer r.Close()
	var chunk [512]byte
	n, _ := io.ReadFull(r, chunk[:])
	ctype = http.DetectContentType(chunk[:n])
	z.rw.Lock()
	z.mimeCache[f] = ctype
	z.rw.Unlock()
	return ctype
}
