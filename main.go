package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	paths "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var (
	// list of directories that are allowed to be accessed
	directories []string
	// list of servers that are proxied
	servers []*server
	// html template for the index page
	index *template.Template
)

// represents a server that we are proxying files from
type server struct {
	name string      // name of the server, used in the url
	url *url.URL     // sftp:// url of the server
	ssh *ssh.Client  // ssh connection to the server
	ftp *sftp.Client // sftp connection to the server
}

// closes the server's connections
func (s *server) close() error {
	ftpErr := s.ftp.Close()
	sshErr := s.ssh.Close()
	return errors.Join(ftpErr, sshErr)
}

// opens a file for reading
func (s *server) open(path string) (*sftp.File, error) {
	return s.ftp.Open(path)
}

// lists the contents of a directory
func (s *server) list(path string) ([]os.FileInfo, error) {
	return s.ftp.ReadDir(path)
}

// represents a user's query for a server's file or directory
type query struct {
	path           string  // cleaned input path
	resolvedServer *server // resolved server
	resolvedPath   string  // resolved path in server's file system
	isRoot         bool    // true if this query is for either the proxy's root directory or a server's root directory
	isBlocked      bool    // true if this query should be blocked
	isCompressed   bool    // true if this query should be served with bzip2 compression
}

// true if this query is for nothing, no server, the proxy's root directory, etc.
func (q *query) isProxyRoot() bool {
	return q.path == ""
}

// true if this query is valid, has a server and is not blocked
func (q *query) isValid() bool {
	return q.resolvedServer != nil && !q.isBlocked
}

// resolves a request url path into a query object
func resolve(path string) (q query) {
	// start with the raw input path
	q.path = path

	// clean the input path up, remove leading and trailing slashes
	q.path = paths.Clean("/"+q.path)
	q.path = strings.Trim(q.path, "/")

	// special case for empty path
	// first two checks should always be false but just in case
	if q.path == "" || q.path == "/" || q.path == "." {
		q.path = ""
		q.isRoot = true
		return
	}

	// split the input path into server name, first directory, and the rest of the path
	parts := strings.SplitN(q.path, "/", 3)

	// special case for server's root directory
	if len(parts) < 2 {
		parts = []string{q.path, ""}
	}

	// select the server by name
	for _, s := range servers {
		if s.name == parts[0] {
			q.resolvedServer = s
			break
		}
	}
	if q.resolvedServer == nil { return }

	// join the first directory and the rest of the path together
	q.resolvedPath = paths.Join(parts[1:]...)

	// detect compression extension, if present, and flag then remove it
	q.resolvedPath, q.isCompressed = strings.CutSuffix(q.resolvedPath, ".bz2")

	// resolve the path relative to the server's configured base path
	q.resolvedPath = paths.Join(q.resolvedServer.url.Path, q.resolvedPath)

	// check if there is no first directory (server's root directory)
	q.isRoot = parts[1] == ""

	// check if the first directory is in the list of allowed directories
	q.isBlocked = !q.isRoot && !slices.Contains(directories, parts[1])

	return
}

// represents an indexable file or directory
type entry interface {
	Name() string       // base name
	Size() int64        // size in bytes
	ModTime() time.Time // modification time
	IsDir() bool        // true if this entry is a directory
}

func (s *server) Name() string { return s.name }
func (s *server) Size() int64 { return 0 }
func (s *server) ModTime() time.Time { return time.Time{} }
func (s *server) IsDir() bool { return true }

// compiles the index page template
func compile() {
	const text =
`<!DOCTYPE html>
<html>
<head><title>foxfastdl /{{.Path}}</title></head>
<body>
  <h1>Index of /{{.Path}}</h1>
  <table>
    <tr>
      <th>Name</th>
      <th>Size</th>
      <th>Modified</th>
    </tr>
    <tr><th colspan="3"><hr></th></tr>
    {{if .Path}}
    <tr>
      <td><a href="..">..</a></td>
      <td>&nbsp;</td>
      <td>&nbsp;</td>
    </tr>
    {{end}}
    {{range .Entries}}
    <tr>
      <td><a href="{{.Name}}{{if .IsDir}}/{{end}}">{{.Name}}{{if .IsDir}}/{{end}}</a></td>
      <td align="right">{{if .IsDir}}-{{else}}{{.Size}}{{end}}</td>
      <td align="right">{{.ModTime.Format "2006-01-02 15:04"}}</td>
    </tr>
    {{end}}
    <tr><th colspan="3"><hr></th></tr>
  </table>
  <p>Powered by <a href="https://github.com/luavixen/foxfastdl">foxfastdl</a> &#128062;</p>
</body>
</html>
`
	index = template.Must(template.New("index").Parse(text))
}

// renders an index page for a set of entries
func render[T any](w http.ResponseWriter, r *http.Request, query *query, entries []T) {
	w.Header().Set("Cache-Control", "max-age=30, public")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// only actually render on GET requests
	if r.Method == "GET" {
		// prepare the options for the index page template
		var options struct {
			Path string
			Entries []entry
		}
		options.Path = query.path
		options.Entries = make([]entry, len(entries))
		// convert the entries to the entry interface
		for i, e := range entries {
			options.Entries[i] = any(e).(entry)
		}
		// filter out directories that are not in the list of allowed directories
		if query.isRoot && !query.isProxyRoot() {
			options.Entries = slices.Collect(func(yield func(entry) bool) {
				for _, e := range options.Entries {
					for _, dir := range directories {
						if e.Name() == dir {
							if !yield(e) { return }
						}
					}
				}
			})
		}
		// sort the entries by name, directories first
		slices.SortFunc(options.Entries, func(a, b entry) int {
			if a.IsDir() == b.IsDir() {
				return strings.Compare(a.Name(), b.Name())
			}
			if a.IsDir() {
				return -1
			} else {
				return 1
			}
		})
		// render the index page
		err := index.Execute(w, options)
		if err != nil {
			log.Printf("render failed: %v\n", err)
		}
	}
}

// sends an HTTP error to the client
func sendErrorCode(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

// sends an error to the client
func sendError(w http.ResponseWriter, cause string, err error) {
	if errors.Is(err, os.ErrNotExist) {
		sendErrorCode(w, http.StatusNotFound)
		return
	}
	if errors.Is(err, os.ErrPermission) {
		sendErrorCode(w, http.StatusForbidden)
		return
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || os.IsTimeout(err) {
		sendErrorCode(w, http.StatusGatewayTimeout)
		return
	}
	sendErrorCode(w, http.StatusInternalServerError)
	log.Printf("%s failed: %v\n", cause, err)
}

// serves a request
func serve(w http.ResponseWriter, r *http.Request) {
	// only GET and HEAD requests are allowed
	if r.Method != "GET" && r.Method != "HEAD" {
		sendErrorCode(w, http.StatusMethodNotAllowed)
		return
	}

	// resolve the request path into a query object
	query := resolve(r.URL.Path)

	// log the request
	log.Printf("%s /%s\n", r.Method, query.path)

	// special case for root directory
	if query.isProxyRoot() {
		render(w, r, &query, servers)
		return
	}

	// special case for invalid query
	if !query.isValid() {
		sendErrorCode(w, http.StatusNotFound)
		return
	}

	server := query.resolvedServer
	path := query.resolvedPath

	// open the remote file or directory
	file, err := server.open(path)
	if err != nil {
		sendError(w, "open", err)
		return
	}
	defer file.Close()

	// get the file's metadata
	stat, err := file.Stat()
	if err != nil {
		sendError(w, "stat", err)
		return
	}

	// render an index page if directory,
	// otherwise serve the file
	if stat.IsDir() {
		entries, err := server.list(path)
		if err != nil {
			sendError(w, "list", err)
		} else {
			render(w, r, &query, entries)
		}
	} else {
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(stat.Name())))
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
		w.Header().Set("Last-Modified", stat.ModTime().UTC().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=30, must-revalidate, public")
		w.WriteHeader(http.StatusOK)
		file.WriteTo(w)
	}
}

// gets an environment variable or a default value
func env(k string, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	} else {
		return d
	}
}

func main() {
	log.Println("foxfastdl v0.2.0 (c) 2025 Lua MacDougall <lua@foxgirl.dev>")
	compile()
	godotenv.Load()
	directories = strings.Split(env("FASTDL_PATHS", "maps,materials,models,sound,particles,scripts,resource"), ",")
	servers = make([]*server, 0, 10)
	defer func() {
		for _, s := range servers {
			s.close()
		}
	}()
	for i := range ^uint(0) {
		k := fmt.Sprintf("FASTDL_SERVER%d", i+1)
		v := env(k, "")
		if v == "" { break }
		u, err := url.Parse(v)
		if err != nil {
			log.Printf("invalid %s: %v\n", k, err)
		}
		if u.Scheme != "sftp" {
			log.Printf("invalid scheme for %s, expected sftp\n", k)
			continue
		}
		n := u.Query().Get("name")
		if n == "" {
			n = u.Hostname()
		}
		host := u.Host
		user := u.User.Username()
		pass, _ := u.User.Password()
		cfg := ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{ssh.Password(pass)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		ssh, err := ssh.Dial("tcp", host, &cfg)
		if err != nil {
			log.Printf("dial failed %s: %v\n", n, err)
			time.Sleep(3 * time.Second)
			return
		}
		ftp, err := sftp.NewClient(ssh)
		if err != nil {
			log.Printf("sftp failed %s: %v\n", n, err)
			time.Sleep(3 * time.Second)
			return
		}
		servers = append(servers, &server{name: n, url: u, ssh: ssh, ftp: ftp})
		log.Printf("connected %s at %s\n", n, u.Host)
	}
	bind := env("FASTDL_BIND", ":8080")
	log.Printf("listening on %s\n", bind)
	if err := http.ListenAndServe(bind, http.HandlerFunc(serve)); err != nil {
		log.Printf("listen failed: %v\n", err)
		time.Sleep(3 * time.Second)
	}
}
