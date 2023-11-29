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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

//go:embed template/*
var static embed.FS

var tmpl = template.Must(template.ParseFS(static, "template/*"))

var name *string = flag.String("name", "", "input file path")
var listen *string = flag.String("listen", ":8080", "http listener")

func main() {
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)
	flag.Parse()
	if *name == "" {
		flag.Usage()
		os.Exit(2)
	}
	slog.Info("opening archive", "name", *name)
	rc, err := zip.OpenReader(*name)
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", ZipFS(rc))

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	slog.Info("listening on", "listen", ln.Addr())
	panic(http.Serve(ln, nil))
}

// Wrapper around the zip file which provides HTTP serving with
// precompressed gzip encoding.
type zipFS struct {
	*zip.ReadCloser
	mimeCache map[*zip.File]string
}

func ZipFS(z *zip.ReadCloser) *zipFS {
	return &zipFS{
		z,
		make(map[*zip.File]string),
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

func (z *zipFS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The URL always starts with a /, but z.Open doesn't want that
	// It ends with a / if it's a directory, but z.Open doesn't want that either
	name := strings.Trim(r.URL.Path, "/")
	if name == "" {
		name = "."
	}
	entry, err := z.Find(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer entry.Close()

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
		tmpl.ExecuteTemplate(w, "dir.html", entries)
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
			io.Copy(w, entry)
		}
	}
}

func (z *zipFS) GetMime(f *zip.File) string {
	if x, ok := z.mimeCache[f]; ok {
		return x
	}
	ctype := mime.TypeByExtension(filepath.Ext(f.Name))
	if ctype != "" {
		z.mimeCache[f] = ctype
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
	z.mimeCache[f] = ctype
	return ctype
}
