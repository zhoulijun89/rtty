package cmd

import (
	"context"
	"crypto/md5"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"
	"unicode/utf8"

	_ "embed"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"golang.org/x/net/websocket"
)

//go:embed public/login.html
var loginHTML string

//go:embed public/index.html
var indexHTML string

//go:embed public/index.js
var indexJS string

//go:embed public
var public embed.FS

// run command
var command string = getenv("SHELL", "bash")

// wait time for server start
var waitTime = 500
var checkProcInterval = 5

type Event string

const (
	EventResize  Event = "resize"
	EventSnedkey Event = "sendKey"
	EventClose   Event = "close"
)

type Message struct {
	Event Event
	Data  interface{}
}

var ptmx *os.File
var execCmd *exec.Cmd

func run(ws *websocket.Conn) {
	defer ws.Close()

	wsconn := &wsConn{
		conn: ws,
	}

	if ptmx == nil {
		var msg Message
		if err := json.NewDecoder(ws).Decode(&msg); err != nil {
			log.Println("failed to decode message:", err)
			return
		}

		rows, cols, err := windowSize(msg.Data)
		if err != nil {
			_, _ = ws.Write([]byte(fmt.Sprintf("%s\r\n", err)))
			return
		}
		winsize := &pty.Winsize{
			Rows: rows,
			Cols: cols,
		}

		c := filter(strings.Split(command, " "))
		if len(c) > 1 {
			//nolint
			execCmd = exec.Command(c[0], c[1:]...)
		} else {
			//nolint
			execCmd = exec.Command(c[0])
		}

		ptmx, err = pty.StartWithSize(execCmd, winsize)
		if err != nil {
			log.Println("failed to create pty", err)
			return
		}
	}

	// write data to process
	go func() {
		for {
			var msg Message
			if err := json.NewDecoder(ws).Decode(&msg); err != nil {
				return
			}

			if msg.Event == EventClose {
				log.Println("close websocket")
				ws.Close()
				return
			}

			if msg.Event == EventResize {
				rows, cols, err := windowSize(msg.Data)
				if err != nil {
					log.Println(err)
					return
				}

				winsize := &pty.Winsize{
					Rows: rows,
					Cols: cols,
				}

				if err := pty.Setsize(ptmx, winsize); err != nil {
					log.Println("failed to set window size:", err)
					return
				}
				continue
			}

			data, ok := msg.Data.(string)
			if !ok {
				log.Println("invalid message data:", data)
				return
			}

			_, err := ptmx.WriteString(data)
			if err != nil {
				log.Println("failed to write data to ptmx:", err)
				return
			}
		}
	}()

	_, _ = io.Copy(wsconn, ptmx)
}

type wsConn struct {
	conn *websocket.Conn
	buf  []byte
}

// Checking and buffering `b`
// If `b` is invalid UTF-8, it would be buffered
// if buffer is valid UTF-8, it would write to connection
func (ws *wsConn) Write(b []byte) (i int, err error) {
	if !utf8.Valid(b) {
		buflen := len(ws.buf)
		blen := len(b)
		ws.buf = append(ws.buf, b...)[:buflen+blen]
		if utf8.Valid(ws.buf) {
			_, e := ws.conn.Write(ws.buf)
			ws.buf = ws.buf[:0]
			return blen, e
		}
		return blen, nil
	}

	if len(ws.buf) > 0 {
		n, err := ws.conn.Write(ws.buf)
		ws.buf = ws.buf[:0]
		if err != nil {
			return n, err
		}
	}
	n, e := ws.conn.Write(b)
	return n, e
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run command",
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) > 0 {
			command = args[0]
		}
		portFlag, err := cmd.PersistentFlags().GetInt("port")
		if err != nil {
			log.Println(err)
			return
		}
		port := strconv.Itoa(portFlag)

		font, err := cmd.PersistentFlags().GetString("font")
		if err != nil {
			log.Println(err)
			return
		}
		fontSize, err := cmd.PersistentFlags().GetString("font-size")
		if err != nil {
			log.Println(err)
			return
		}

		addr, err := cmd.PersistentFlags().GetString("addr")
		if err != nil {
			log.Println(err)
			return
		}
		if addr == "" {
			addr = "localhost"
		}

		indexJS = strings.Replace(indexJS, "{addr}", template.JSEscapeString(addr), 1)
		indexJS = strings.Replace(indexJS, "{port}", port, 1)
		indexJS = strings.Replace(indexJS, "{fontFamily}", template.JSEscapeString(font), 1)
		indexJS = strings.Replace(indexJS, "{fontSize}", template.JSEscapeString(fontSize), 1)

		var serverErr error
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(loginHTML))
		})
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			password := r.PostFormValue("password")
			key := "root_" + time.Now().Format("2006-01-02")
			md5Str := fmt.Sprintf("%x", md5.Sum([]byte(key)))
			log.Println(fmt.Sprintf("密钥为：%s,加密后为：%s", key, md5Str))
			if password != md5Str {
				http.Error(w, "用户名或密码错误!", 401)
				return
			}
			_, _ = w.Write([]byte(indexHTML))
		})
		sub, err := fs.Sub(public, "public")
		if err != nil {
			return
		}
		publicHandler := http.FileServer(http.FS(sub))
		mux.Handle("/css/", publicHandler)
		mux.Handle("/js/", publicHandler)
		mux.HandleFunc("/index.js", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(indexJS))
		})
		mux.Handle("/ws", websocket.Handler(run))

		server := &http.Server{
			Addr:    addr + ":" + port,
			Handler: mux,
		}

		go func() {
			log.Println("running command: " + command)
			log.Printf("running http://%s:%s\n", addr, port)

			if serverErr := server.ListenAndServe(); serverErr != nil {
				log.Println(serverErr)
			}
		}()

		// check process state
		go func() {
			ticker := time.NewTicker(time.Duration(checkProcInterval) * time.Second)
			for range ticker.C {
				if execCmd != nil {
					state, err := execCmd.Process.Wait()
					if err != nil {
						return
					}

					if state.ExitCode() != -1 {
						ptmx.Close()
						ptmx = nil
						execCmd = nil
					}
				}
			}
		}()

		// wait for run server
		time.Sleep(time.Duration(waitTime) * time.Microsecond)

		if serverErr == nil {
			// open browser
			openView, err := cmd.PersistentFlags().GetBool("view")
			if err != nil {
				log.Println(err)
			} else if openView {
				OpenBrowser(fmt.Sprintf("http://%s:%s", addr, port))
			}
		}

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM, os.Interrupt)
		<-quit

		if ptmx != nil {
			_ = ptmx.Close()
		}
		if execCmd != nil {
			_ = execCmd.Process.Kill()
			_, _ = execCmd.Process.Wait()
		}
		if err := server.Shutdown(context.Background()); err != nil {
			log.Println("failed to shutdown server", err)
		}
	},
}

func init() {
	runCmd.PersistentFlags().IntP("port", "p", 9999, "server port")
	runCmd.PersistentFlags().StringP("addr", "a", "127.0.0.1", "server address")
	runCmd.PersistentFlags().String("font", "", "font")
	runCmd.PersistentFlags().String("font-size", "", "font size")
	runCmd.PersistentFlags().BoolP("view", "v", false, "open browser")
	runCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Printf(`Run command

Usage:
  rtty run [command] [flags]

Command
  Execute specified command (default "%s")

Flags:
  -a, --addr string        server address
      --font string        font
      --font-size string   font size
  -h, --help               help for run
  -p, --port int           server port (default 9999)
  -v, --view               open browser
`, command)
	})
	rootCmd.AddCommand(runCmd)
}
