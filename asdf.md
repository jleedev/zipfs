zip.OpenReader takes the path to a zip file
calls Reader.init to initialize r.File
which is an array of just the entries found in the archive on disk

when you actually go to open a file
(by calling zip.Reader.Open)
it builds a secondary, private structure r.fileList
zip.Reader.initFileList scans through the File entries in the zip file
and splits directories and makes sure things are sorted and unique
so that it can perform fast lookups
a valid zip file should have entries for directories but it looks like this
will synthesize them even if they're missing.

so zip.Reader.Open delegates to a private function openLookup
it splits the directory from the basename of the requested entry
and returns some *fileListEntry or nil
this fileListEntry could embed a File, or not.
and then zip.Reader.Open calls file.Open() on the resulting thing
and you can only read the uncompressed bytes out of it, it won't give you
the File entry.

you can process a zip file yourself by range over the File[], but i
really want to take advantage of things like, looking up a file by name
quickly, not to mention scanning a subdirectory.

maybe i'll ask it to open "/" and see what happens.

...

horrible tricks
call zip.Reader.Open to get the fs.File
immediately close it to avoid leaking a decoder i guess
use reflection to either get its .e.file (if it's a directory)
or its .f (if it's a file) which is the underlying *zip.File.
it can be nil if it's a directory such as "." or one that was synthesized.

...

and then to actually build an http server on this, umm

the other thing we need to do is implement the semantics which connect
the fs.FS to the http.FileServer

http.FileServer does a bunch of stuff for free
something about index.html
etag, conditional, modtime etc
mime type guessing by filepath.Ext
and by reading 512 bytes to call http.DetectContentType
maybe some path normalization but that'll probably work itself out
range requests i guess
if it's a directory then redirect you with a /

more things http.FileServer does:

```html
<pre>
<a href="Extra/">Extra/</a>
<a href="License.txt">License.txt</a>
</pre>
```

gives a directory listing and indicates
which are files and which are directories

sets Last-Modified on a file response
does a 301 on a directory without a trailing slash
hoho, sets Last-Modified on a directory response

```bash
jleedev@penguin:~/junk/zipfs¶ ./zipfs -name ~/TheUnarchiverSource.zip -listen :8000 -fs=std
jleedev@penguin:~/junk/zipfs¶ curl http://localhost:8000/Extra/lsar.1
seeker can't seek
```

🧐 ohh my goodness

https://go.googlesource.com/website/+/06fdb770f723cf562f31a737e2ba78d438a15415
https://go.dev/cl/329249
cmd/golangorg: make zip contents seekable

https://go.dev/issue/46809
x/website: "seeker can't seek" error when serving particular files

so they wrap their zip in &seekableFS{z} which will just call ioutil.ReadAll
any time a regular file is opened so that the http server can seek it

i feel like i'd rather not do that

maybe just peel off 512 bytes of uncompressed data for sniffing usage
hell just do that once for each file requested and cache it
actually just determine the mime type, and cache that
so you don't have to do that on every request
just the first request for each file

ok so there are some things that http.FileServer does which we really don't
need it to do. rather, the way it uses the fs.FS isn't what we want.

example
//Extra -> /Extra -> /Extra/
figure out if one of those is http.Server and one is http.FileServer

when serving a directory, if the request url doesn't end in /
then do a redirect

when serving a directory, if /index.html exists then use that
otherwise spit out a directory listing

when a request is for /index.html, redirect to without that

lot going on here:
a zip entry is (by convention) a directory if it ends in a /
and also has a size of 0
but zip.Reader.Open refuses to accept any name that ends in a /
since it has processed this into a directory structure
