package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dsnet/compress/bzip2"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func env(k string, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	} else {
		return d
	}
}

type server struct {
	name string
	url *url.URL
	ssh *ssh.Client
	ftp *sftp.Client
}

var (
	bind string
	paths []string
	servers []*server
)

func splitPath(p string) (string, string) {
	p = path.Clean("/"+p)
	p = strings.TrimPrefix(p, "/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	} else {
		return p, ""
	}
}

func resolvePath(s *server, a string) (string, bool) {
	p := a
	p = path.Join(s.url.Path, p)
	p = strings.TrimPrefix(p, "/")
	return strings.CutSuffix(p, ".bz2")
}

func blockPath(a string) bool {
	parts := strings.SplitN(a, "/", 2)
	return !slices.Contains(paths, parts[0])
}

func selectServer(n string) *server {
	for _, s := range servers {
		if s.name == n {
			return s
		}
	}
	return nil
}

func serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, a := splitPath(r.URL.Path)
	if n == "" {
		w.Header().Set("Cache-Control", "max-age=86400, public")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == "GET" {
			fmt.Fprintf(w, "<html><body><h1>Server index</h1><ul>")
			for _, s := range servers {
				fmt.Fprintf(w, "<li><a href=\"/%s\">%s</a></li>", s.name, s.name)
			}
			fmt.Fprintf(w, "</ul><p>powered by <a href=\"https://foxgirl.dev\">my big fat paws</a> :3c</p></body></html>")
		}
		return
	}
	if a != "" && blockPath(a) {
		http.NotFound(w, r)
		return
	}
	s := selectServer(n)
	if s == nil {
		http.NotFound(w, r)
		return
	}
	p, b := resolvePath(s, a)
	log.Printf("%s %s %s bz2? %t", r.Method, n, p, b)
	i, err := s.ftp.Stat(p)
	if err != nil {
		log.Printf("ftp stat failed %s: %v\n", p, err)
		http.NotFound(w, r)
		return
	}
	if i.IsDir() && !b {
		e, err := s.ftp.ReadDir(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if a == "" {
			// filter out files that are not in the paths list
			e = slices.Collect(func(yield func(os.FileInfo) bool) {
				for _, n := range e {
					for _, p := range paths {
						if n.Name() == p {
							if !yield(n) { return }
						}
					}
				}
			})
		}
		sort.Slice(e, func(i, j int) bool {
			return e[i].Name() < e[j].Name()
		})
		w.Header().Set("Cache-Control", "max-age=300, public")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><h1>Index of /%s/%s</h1><ul>", n, a)
		for _, e := range e {
			name := e.Name()
			href := path.Join(n, a, name)
			if e.IsDir() {
				fmt.Fprintf(w, "<li><a href=\"/%s/\">%s/</a></li>", href, name)
			} else {
				fmt.Fprintf(w, "<li><a href=\"/%s\">%s</a> %d bytes</li>", href, name, e.Size())
			}
		}
		fmt.Fprintf(w, "</ul><p>powered by <a href=\"https://foxgirl.dev\">my big fat paws</a> :3c</p></body></html>")
		return
	}
	f, err := s.ftp.Open(p)
	if err != nil {
		log.Printf("ftp open failed %s: %v\n", p, err)
		http.Error(w, "500 internal server error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "max-age=86400, public")
	if b {
		w.Header().Set("Content-Type", "application/x-bzip2")
		if r.Method == "GET" {
			// current impl cant fail, discard err
			z, _ := bzip2.NewWriter(w, nil)
			defer z.Close()
			_, err = io.Copy(z, f)
			if err != nil {
				log.Printf("copy failed %s: %v\n", p, err)
				http.Error(w, "500 internal server error", http.StatusInternalServerError)
				return
			}
		}
	} else {
		// thanks stdlib
		http.ServeContent(w, r, i.Name(), i.ModTime(), f)
	}
}

func main() {
	log.Println("foxfastdl v0.1.0 (c) 2025 Lua MacDougall <lua@foxgirl.dev>")
	bind = env("FASTDL_BIND", ":8080")
	paths = strings.Split(env("FASTDL_PATHS", "maps,materials,models,sound,particles,scripts,resource"), ",")
	servers = make([]*server, 0, 10)
	defer func() {
		for _, s := range servers {
			s.ftp.Close()
			s.ssh.Close()
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
		servers = append(servers, &server{n, u, ssh, ftp})
		log.Printf("connected %s at %s\n", n, u.Host)
	}
	log.Printf("listening on %s\n", bind)
	if err := http.ListenAndServe(bind, http.HandlerFunc(serve)); err != nil {
		log.Printf("listen failed: %v\n", err)
		time.Sleep(3 * time.Second)
	}
}
