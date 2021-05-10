package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"

	_ "embed"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"golang.org/x/net/websocket"
)

//go:embed public/index.html
var indexHTML string

type InitData struct {
	WindowSize struct {
		Width  uint16 `json:"width"`
		Height uint16 `json:"height"`
	} `json:"window_size"`
	Cmd string `json:"cmd"`
}

func run(ws *websocket.Conn) {
	defer ws.Close()

	var data InitData
	if err := json.NewDecoder(ws).Decode(&data); err != nil {
		_, _ = ws.Write([]byte(fmt.Sprintf("failed to decode json: %s\r\n", err)))
		return
	}

	// Create arbitrary command.
	c := exec.Command(data.Cmd)

	// Start the command with a pty.
	ptmx, err := pty.Start(c)
	if err != nil {
		_, _ = ws.Write([]byte(fmt.Sprintf("failed to creating pty: %s\r\n", err)))
		return
	}

	// Make sure to close the pty at the end.
	defer func() {
		_ = ptmx.Close()
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	}() // Best effort.

	// Update pty window size
	winsize := &pty.Winsize{
		Rows: data.WindowSize.Height,
		Cols: data.WindowSize.Width,
		X:    0,
		Y:    0,
	}
	pty.Setsize(ptmx, winsize)

	go func() {
		_, _ = io.Copy(ptmx, ws)
	}()
	_, _ = io.Copy(ws, ptmx)
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run server",
	Run: func(cmd *cobra.Command, args []string) {
		port, err := cmd.PersistentFlags().GetString("port")
		if err != nil {
			log.Println(err)
			return
		}
		html := strings.Replace(indexHTML, "{port}", port, 1)
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(html))
		})
		http.Handle("/ws", websocket.Handler(run))

		log.Println("start server with port: " + port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Println(err)
		}
	},
}

func init() {
	runCmd.PersistentFlags().StringP("port", "p", "9999", "server port")
	rootCmd.AddCommand(runCmd)
}
