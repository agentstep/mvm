package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// newExecDirectCmd creates the hidden exec-direct command.
// This runs INSIDE Lima and does direct TCP to the guest agent.
// Used as a fallback when the daemon is not available.
func newExecDirectCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "exec-direct <guestIP> <base64command>",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			guestIP := args[0]
			commandB64 := args[1]

			command, err := base64.StdEncoding.DecodeString(commandB64)
			if err != nil {
				return fmt.Errorf("invalid base64 command: %w", err)
			}

			return runExecDirect2(guestIP, string(command))
		},
	}
}

// runExecDirect2 does direct TCP to the guest agent protocol.
// Length-prefixed JSON request, length-prefixed JSON response.
func runExecDirect2(guestIP, command string) error {
	conn, err := net.DialTimeout("tcp", guestIP+":5123", 5*time.Second)
	if err != nil {
		return fmt.Errorf("agent not reachable at %s:5123: %w", guestIP, err)
	}
	defer conn.Close()

	// Send exec_stream request (streaming — output arrives in real-time)
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"type": "exec_stream",
		"id":   "e",
		"exec": map[string]string{"command": command},
	})

	length := make([]byte, 4)
	length[0] = byte(len(reqJSON) >> 24)
	length[1] = byte(len(reqJSON) >> 16)
	length[2] = byte(len(reqJSON) >> 8)
	length[3] = byte(len(reqJSON))
	conn.Write(length)
	conn.Write(reqJSON)

	// Read streaming frames — output as they arrive
	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Minute))

		respLen := make([]byte, 4)
		if _, err := readFullConn(conn, respLen); err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		size := int(respLen[0])<<24 | int(respLen[1])<<16 | int(respLen[2])<<8 | int(respLen[3])
		respData := make([]byte, size)
		if _, err := readFullConn(conn, respData); err != nil {
			return fmt.Errorf("read response body: %w", err)
		}

		var resp struct {
			Type     string `json:"type"`
			Data     []byte `json:"data"`
			ExitCode int    `json:"exit_code"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(respData, &resp); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		switch resp.Type {
		case "stdout":
			os.Stdout.Write(resp.Data)
		case "stderr":
			os.Stderr.Write(resp.Data)
		case "exit":
			if resp.ExitCode != 0 {
				os.Exit(resp.ExitCode)
			}
			return nil
		case "error":
			return fmt.Errorf("agent: %s", resp.Error)
		}
	}
}

func readFullConn(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
