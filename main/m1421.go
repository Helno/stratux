package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/stratux/serial"
)

/*
	m1421.go: ADSBee 1421 dual-band (1090ES + 978 UAT) receiver, connected over USB through the
	ADSBee 1421 Programmer (a USB CDC serial bridge, udev symlink /dev/adsbee1421).

	The module's console runs at a fixed 1,000,000 baud (changing it can break the programmer bridge).
	We select the RAW reporting protocol, which emits text lines:
		#MDS*<hex>;(<src>,<dBm>,<sigq>,<mlat>)    1090 Mode S / ADS-B
		#UAT*-<hex>;(<dBm>,<sigq>,<mlat>)         978 UAT ADS-B / TIS-B
		#UAT*+<hex>;(<dBm>,<sigq>,<mlat>)         978 UAT uplink (FIS-B), interleaved + uncorrected
*/

const (
	m1421Device       = "/dev/adsbee1421"
	m1421Baud         = 1000000
	m1421Dump1090Addr = "127.0.0.1:30001"
	m1421KeepAlive    = 30 * time.Second
)

var m1421Commands = []string{
	"AT+LOG_LEVEL=SILENT",
	"AT+PROTOCOL_OUT=CONSOLE,RAW",
}

func initM1421Serial(isTraceReplayMode bool) error {
	if isTraceReplayMode {
		return nil
	}

	go func() {
		for {
			time.Sleep(2 * time.Second)

			if !globalSettings.M1421_Enabled || globalStatus.M1421_connected {
				continue
			}
			if _, err := os.Stat(m1421Device); err != nil {
				continue
			}

			port, err := openM1421()
			if err != nil {
				log.Printf("M1421: %s\n", err.Error())
				continue
			}

			log.Printf("M1421 connected on %s\n", m1421Device)
			globalStatus.M1421_connected = true
			go m1421Run(port)
		}
	}()

	return nil
}

func openM1421() (*serial.Port, error) {
	port, err := serial.OpenPort(&serial.Config{Name: m1421Device, Baud: m1421Baud})
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", m1421Device, err)
	}

	// The programmer negotiates the console baud rate with the module after enumeration and
	// treats anything received meanwhile as noise, so stay quiet briefly before the first command.
	time.Sleep(2 * time.Second)

	if err := sendM1421Commands(port); err != nil {
		port.Close()
		return nil, err
	}
	return port, nil
}

func sendM1421Commands(port *serial.Port) error {
	for _, cmd := range m1421Commands {
		if _, err := port.Write([]byte(cmd + "\r\n")); err != nil {
			return fmt.Errorf("failed to send %q: %w", cmd, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func startM1421Dump1090() (*exec.Cmd, net.Conn, error) {
	cmd := exec.Command(STRATUX_HOME+"/bin/dump1090", "--net-only", "--net-stratux-port", "30006")
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start dump1090: %w", err)
	}

	var lastErr error
	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", m1421Dump1090Addr)
		if err == nil {
			return cmd, conn, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()
	return nil, nil, fmt.Errorf("dump1090 not reachable: %w", lastErr)
}

func m1421Run(port *serial.Port) {
	defer func() {
		globalStatus.M1421_connected = false
		port.Close()
		log.Printf("M1421 disconnected\n")
	}()

	dump1090Cmd, dump1090Conn, err := startM1421Dump1090()
	if err != nil {
		log.Printf("M1421: %s\n", err.Error())
		return
	}
	defer func() {
		dump1090Conn.Close()
		dump1090Cmd.Process.Kill()
		dump1090Cmd.Wait()
	}()

	// If the module resets it falls back to its saved reporting protocol, so reassert ours.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(m1421KeepAlive)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sendM1421Commands(port)
			}
		}
	}()

	scanner := bufio.NewScanner(port)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() && globalSettings.M1421_Enabled {
		if err := m1421HandleLine(strings.TrimSpace(scanner.Text()), dump1090Conn); err != nil {
			log.Printf("M1421: %s\n", err.Error())
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("M1421 serial port error: %s\n", err.Error())
	}
}

// m1421HandleLine forwards one RAW line into the Stratux pipeline; all other lines (command replies, logs) are ignored.
func m1421HandleLine(line string, dump1090Conn net.Conn) error {
	switch {
	case strings.HasPrefix(line, "#MDS*"):
		report, ok := m1421ReportData(line[len("#MDS*"):])
		if !ok {
			return nil
		}
		globalStatus.ES_messages_total++
		if _, err := dump1090Conn.Write([]byte("*" + report + ";\r\n")); err != nil {
			return fmt.Errorf("dump1090 write failed: %w", err)
		}

	case strings.HasPrefix(line, "#UAT*-"):
		report, ok := m1421ReportData(line[len("#UAT*-"):])
		if !ok {
			return nil
		}
		// Short reports arrive with their 12 FEC bytes; Stratux expects the 18 byte payload only.
		if len(report) == 60 {
			report = report[:36]
		}
		if o, msgtype := parseInput("-" + report + ";"); o != nil && msgtype != 0 {
			relayMessage(msgtype, o)
		}
	}
	// "#UAT*+" uplinks (FIS-B) are not handled yet: RAW carries them interleaved and uncorrected.
	return nil
}

// m1421ReportData returns the hex payload up to the ';' that precedes the metadata tuple.
func m1421ReportData(s string) (string, bool) {
	end := strings.IndexByte(s, ';')
	if end <= 0 {
		return "", false
	}
	return s[:end], true
}
