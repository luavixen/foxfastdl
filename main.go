package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	paths "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var (
	// magic password header value or empty string if not set
	password string
	// list of directories that are allowed to be accessed
	directories []string
	// list of servers that are proxied
	servers []*server
	// html template for the index page
	index *template.Template
)

// represents a server that we are proxying files from
type server struct {
	url  *url.URL // url of the server, like sftp://user:pass@host:port/path?name=NAME
	name string   // name of the server taken from the url

	ctx context.Context // server deadline

	mu sync.Mutex // mutex protecting the `conn` field
	conn   *conn  // current connection to the server, or nil
}

// represents an open sftp connection to a server
type conn struct {
	ssh *ssh.Client  // ssh connection to the server
	ftp *sftp.Client // sftp connection to the server
}

// closes the connection
func (c *conn) close() error {
	ftpErr := c.ftp.Close()
	sshErr := c.ssh.Close()
	return errors.Join(ftpErr, sshErr)
}

// closes the server's connections
func (s *server) close() error {
	// lock the mutex
	s.mu.Lock()
	defer s.mu.Unlock()
	// close the connection if present
	if s.conn != nil {
		return s.conn.close()
	} else {
		return nil
	}
}

// opens a new connection to the server without saving or persisting it
// should only be called by server.connect
func (s *server) dial() (*conn, error) {
	u := s.url
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
		return nil, fmt.Errorf("dial failed %s: %w", s.name, err)
	}
	ftp, err := sftp.NewClient(ssh)
	if err != nil {
		return nil, fmt.Errorf("sftp failed %s: %w", s.name, err)
	}
	return &conn{ssh, ftp}, nil
}

// gets a connection to the server
// - if the server does not have a connection: a new connection will be opened
// - if the server already has a connection, and it is equal to the one provided: it will be closed, and a new connection will be opened
// - if the server already has a connection, and it is not equal to the one provided: it will be returned
func (s *server) connect(not *conn) (*conn, error) {
	// check the server's deadline
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	// lock the mutex
	s.mu.Lock()
	defer s.mu.Unlock()
	// check if the server already has a connection
	if s.conn != nil {
		// if the connection is not equal to the one provided, return it
		if s.conn != not {
			return s.conn, nil
		}
		// if the connection is equal to the one provided, close it
		s.conn.close()
		s.conn = nil
	}
	// open a new connection
	c, err := s.dial()
	if err != nil {
		return nil, err
	} else {
		s.conn = c
		return c, nil
	}
}

// returns true if the error indicates that the connection was lost
func wasConnLost(err error) bool {
	return (
		errors.Is(err, sftp.ErrSSHFxFailure)        ||
		errors.Is(err, sftp.ErrSSHFxBadMessage)     ||
		errors.Is(err, sftp.ErrSSHFxNoConnection)   ||
		errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		errors.Is(err, syscall.ECONNABORTED)        ||   // connection aborted
		errors.Is(err, syscall.ECONNRESET)          ||   // connection reset by peer
		errors.Is(err, syscall.ECONNREFUSED)        ||   // connection refused
		errors.Is(err, syscall.ETIMEDOUT)           ||   // connection timed out
		errors.Is(err, syscall.EPIPE)               ||   // broken pipe
		errors.Is(err, syscall.ENOTCONN)            ||   // socket is not connected
		errors.Is(err, syscall.ENOTSOCK)            ||   // not a socket
		errors.Is(err, syscall.EHOSTUNREACH)        ||   // no route to host
		errors.Is(err, syscall.EHOSTDOWN)           ||   // host is down
		errors.Is(err, syscall.ENETUNREACH)         ||   // network is unreachable
		errors.Is(err, syscall.ENETDOWN)            ||   // network is down
		errors.Is(err, syscall.ENETRESET)           ||   // network dropped connection
		errors.Is(err, syscall.ENODEV)              ||   // no such device
		errors.Is(err, syscall.ENFILE)              ||   // too many open files in system
		strings.Contains(err.Error(), "connection"))
}

// sleeps for a duration
// returns true if the duration elapsed, false if the context was canceled
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false // canceled
	case <-time.After(d):
		return true // completed normally
	}
}

// performs an action with a connection to the server
// THE ACTION MUST BE IDEMPOTENT
// THE ACTION MAY BE CALLED ZERO, ONE, OR TWO TIMES
func (s *server) perform(f func(*conn) error) error {
	// get a connection to the server
	a, err := s.connect(nil)
	if err != nil {
		// connect failed, return immediately
		log.Printf("connect failed %s: %v\n", s.name, err)
		return err
	} else {
		// perform the action
		err := f(a)
		if err != nil {
			// check if the connection was lost
			if wasConnLost(err) {
				// take a breather
				sleep(s.ctx, 5*time.Second)
				// attempt to reconnect to the server
				b, err := s.connect(a)
				if err != nil {
					// reconnect failed, return the error
					log.Printf("connect failed %s: %v\n", s.name, err)
					return err
				} else {
					// reconnect succeeded, try and perform the action a second time
					return f(b)
				}
			} else {
				// action failed, return the error
				return err
			}
		} else {
			// everything went well, return nil :D
			return nil
		}
	}
}

// opens a file for reading
func (s *server) open(path string) (f *sftp.File, err error) {
	err = s.perform(func(c *conn) error {
		f, err = c.ftp.Open(path)
		return err
	})
	return
}

// lists the contents of a directory
func (s *server) list(path string) (e []os.FileInfo, err error) {
	err = s.perform(func(c *conn) error {
		e, err = c.ftp.ReadDir(path)
		return err
	})
	return
}

// monitors the server's connection by listing the root directory in a loop
// if the listing fails, the cancel function is called with the error
func (s *server) monitor(d time.Duration, cancel context.CancelCauseFunc) {
	for {
		_, err := s.list("")
		if err != nil {
			err = fmt.Errorf("monitor failed %s: %w", s.name, err)
			log.Printf("%s\n", err)
			cancel(err)
			return
		}
		sleep(s.ctx, d)
	}
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
  <p>Powered by <a href="https://git.vixen.computer/lua/foxfastdl">foxfastdl</a> &#128062;</p>
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

	// check if the password is set and the header is correct
	if password != "" && r.Header.Get("X-Foxfastdl-Password") != password {
		sendErrorCode(w, http.StatusForbidden)
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
		// write headers based on the file's metadata
		if t := mime.TypeByExtension(filepath.Ext(stat.Name())); t != "" {
			w.Header().Set("Content-Type", t)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
		w.Header().Set("Last-Modified", stat.ModTime().UTC().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=30, must-revalidate, public")
		w.WriteHeader(http.StatusOK)
		// write the file's data to the response
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
	log.SetFlags(0)
	log.Println("foxfastdl v0.3.0 (c) 2025 Lua MacDougall <lua@foxgirl.dev>")
	compile()
	godotenv.Load()
	password = env("FASTDL_PASSWORD", "")
	directories = strings.Split(env("FASTDL_PATHS", "maps,materials,models,sound,particles,scripts,resource,replay"), ",")
	monitor, err := strconv.Atoi(env("FASTDL_MONITOR_SECONDS", "60"))
	if err != nil || monitor < 0 {
		log.Printf("invalid FASTDL_MONITOR_SECONDS: %v\n", err)
		monitor = 60
	}
	servers = make([]*server, 0, 10)
	failures := 0
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(errors.New("exited"))
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		fmt.Println("\ninterrupted, stopping...")
		cancel(errors.New("interrupted"))
	}()
	for i := range ^uint(0) {
		k := fmt.Sprintf("FASTDL_SERVER%d", i+1)
		v := env(k, "")
		if v == "" { break }
		u, err := url.Parse(v)
		if err != nil {
			log.Printf("invalid %s: %v\n", k, err)
			failures++
			continue
		}
		if u.Scheme != "sftp" {
			log.Printf("invalid scheme for %s, expected sftp\n", k)
			failures++
			continue
		}
		n := u.Query().Get("name")
		if n == "" {
			n = u.Hostname()
		}
		s := &server{name: n, url: u, ctx: ctx}
		if _, err := s.connect(nil); err != nil {
			log.Printf("failed to connect to %s: %v\n", n, err)
			failures++
			continue
		}
		context.AfterFunc(ctx, func() { go s.close() })
		servers = append(servers, s)
		log.Printf("connected %s at %s\n", n, u.Host)
	}
	if failures > 0 {
		log.Printf("failed to connect to %d servers\n", failures)
		return
	}
	if monitor > 0 {
		for _, s := range servers {
			go s.monitor(time.Duration(monitor)*time.Second, cancel)
		}
	}
	bind := env("FASTDL_BIND", ":8080")
	log.Printf("listening on %s\n", bind)
	h := &http.Server{
		Addr: bind,
		Handler: http.HandlerFunc(serve),
		BaseContext: func(l net.Listener) context.Context { return ctx },
	}
	context.AfterFunc(ctx, func() {
		go h.Shutdown(ctx)
		time.Sleep(5*time.Second)
		go h.Close()
		time.Sleep(1*time.Second)
		os.Exit(1)
	})
	if err := h.ListenAndServe(); err != nil {
		log.Printf("listen failed: %v\n", err)
		time.Sleep(3*time.Second)
	}
}
