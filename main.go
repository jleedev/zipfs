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
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"

	"blitiri.com.ar/go/systemd"
)

//go:embed template/*
var static embed.FS

var index *string = flag.String("index", "", "serve <index.html> instead of directory listing")

var tmpl = template.Must(template.ParseFS(static, "template/*"))

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)
	slog.SetLogLoggerLevel(slog.LevelDebug)
	info, _ := debug.ReadBuildInfo()
	log.Printf("%#v", info.Main)

	http.Handle("GET /", NewZipServer())

	l, err := systemd.OneListener("")
	if err != nil {
		log.Fatal("While attempting socket activation: ", err)
	}
	slog.Info("listening on", "listen", fmt.Sprint("http://", l.Addr()))
	panic(fcgi.Serve(l, nil))
}

func NewZipServer() *ZipServer {
	return &ZipServer{
		archives: make(map[string]*zipFS),
	}
}

type ZipServer struct {
	archives map[string]*zipFS
	rw       sync.RWMutex
}

func (z *ZipServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.InfoContext(r.Context(), "begin")
	env := fcgi.ProcessEnv(r)

	path_to_zipfile := env["SCRIPT_FILENAME"]
	z.rw.RLock()
	var zf *zipFS
	var ok bool
	if zf, ok = z.archives[path_to_zipfile]; ok {
		z.rw.RUnlock()
	} else {
		z.rw.RUnlock()
		rc, err := zip.OpenReader(path_to_zipfile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		zf = newZipFS(rc)
		z.rw.Lock()
		z.archives[path_to_zipfile] = zf
		z.rw.Unlock()
	}

	path := strings.Trim(env["PATH_TRANSLATED"], "/")
	if path == "" {
		path = "."
	}
	entry, err := zf.Find(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer entry.Close()
	RespondWith(zf, entry, w, r)
}

// Wrapper around the zip file which provides HTTP serving with
// precompressed gzip encoding.
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

type ZipEntry struct {
	fs.File
	Entry *zip.File
}

// Finds the named File entry in the ZIP archive
// Then do the dumb reflection work to pull out the underlying zip.File
func (z *zipFS) Find(name string) (*ZipEntry, error) {
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

func RespondWith(z *zipFS, entry *ZipEntry, w http.ResponseWriter, r *http.Request) {
	if entry.Entry != nil {
		w.Header().Set("Last-Modified", entry.Entry.Modified.Format(http.TimeFormat))
	}

	// If index.html handling is enabled:
	// - When reading a directory, see if you want to read index.html instead
	// - When reading index.html, redirect to the directory

	if rd, ok := entry.File.(fs.ReadDirFile); ok {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		// Serve the directory listing
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
	} else {
		if entry.Entry == nil {
			panic("impossible")
		}
		w.Header().Set("Content-Type", z.GetMime(entry.Entry))
		if entry.Entry.Method == zip.Deflate && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
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
		} else {
			// Just serve a plain response
			io.Copy(w, entry)
		}
	}
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
