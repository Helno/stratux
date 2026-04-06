/*
	Copyright (c) 2024 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	softrf_mb.go: Integration of Moshe-Braner SoftRF HAT as a subprocess.
	Writes /etc/softrf/settings.txt from globalSettings, launches the SoftRF
	binary, pipes Stratux GPS NMEA into stdin, and reads $PFLAA/$PFLAU from
	stdout into the existing FLARM traffic pipeline.
*/

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const softRFBinaryPath = "/usr/bin/softrf"
const softRFSettingsPath = "/etc/softrf/settings.txt"

// softRFNMEAChan carries GPS NMEA sentences to the SoftRF subprocess stdin.
var softRFNMEAChan = make(chan string, 100)

// softRFRestartChan signals the subprocess manager to restart with new settings.
var softRFRestartChan = make(chan bool, 1)

// softRFPublishNmea is called from gps.go for every valid NMEA sentence.
func softRFPublishNmea(nmea string) {
	if !globalSettings.SoftRFEnabled {
		return
	}
	if !strings.HasSuffix(nmea, "\r\n") {
		nmea += "\r\n"
	}
	select {
	case softRFNMEAChan <- nmea:
	default: // drop if channel is full; GPS updates are frequent enough
	}
}

// softRFSignalRestart asks the running subprocess to restart with updated settings.
// Called from managementinterface.go when SoftRF settings change.
func softRFSignalRestart() {
	select {
	case softRFRestartChan <- true:
	default:
	}
}

func writeSoftRFSettings() error {
	if err := os.MkdirAll("/etc/softrf", 0755); err != nil {
		return err
	}

	// Resolve int fields, applying defaults for unread (-1) values.
	protocol := 7 // FLARM Latest
	if globalSettings.SoftRFProtocol > 0 {
		protocol = globalSettings.SoftRFProtocol
	}
	altProtocol := 8 // ADS-L
	if globalSettings.SoftRFAltProtocol >= 0 {
		altProtocol = globalSettings.SoftRFAltProtocol
	}
	band := 2 // US 915 MHz
	if globalSettings.SoftRFBand > 0 {
		band = globalSettings.SoftRFBand
	}
	txPower := 2 // full
	if globalSettings.SoftRFTxPower >= 0 {
		txPower = globalSettings.SoftRFTxPower
	}
	alarm := 3 // FLARM algorithm
	if globalSettings.SoftRFAlarm >= 0 {
		alarm = globalSettings.SoftRFAlarm
	}
	relay := 1 // relay landed traffic
	if globalSettings.SoftRFRelay >= 0 {
		relay = globalSettings.SoftRFRelay
	}

	acftType := mapAircraftType(typeMappingOgn2SoftRF, true, globalSettings.OGNAcftType)
	if acftType < 0 {
		acftType = 1 // glider default
	}
	idMethod := globalSettings.OGNAddrType
	aircraftID := globalSettings.OGNAddr
	if len(aircraftID) == 0 {
		aircraftID = "000000"
	}

	stealth := 0
	if globalSettings.SoftRFStealth {
		stealth = 1
	}
	noTrack := 0
	if globalSettings.SoftRFNoTrack {
		noTrack = 1
	}

	content := fmt.Sprintf(
		"SoftRF,1\n"+
			"mode,0\n"+
			"protocol,%d\n"+
			"altprotocol,%d\n"+
			"band,%d\n"+
			"acft_type,%d\n"+
			"id_method,%d\n"+
			"aircraft_id,%s\n"+
			"alarm,%d\n"+
			"relay,%d\n"+
			"tx_power,%d\n"+
			"stealth,%d\n"+
			"no_track,%d\n"+
			"nmea_out,1\n"+   // output to stdout
			"nmea_g,00\n"+    // no GPS sentences (Stratux has its own GPS)
			"nmea_s,00\n"+    // no sensor sentences
			"nmea_t,01\n"+    // traffic sentences on ($PFLAA/$PFLAU)
			"nmea_e,00\n"+    // no tunnel
			"nmea_d,00\n"+    // no debug sentences
			"gdl90,0\n"+      // Stratux generates GDL90
			"logflight,0\n",  // no flight logging
		protocol, altProtocol, band,
		acftType, idMethod, aircraftID,
		alarm, relay, txPower,
		stealth, noTrack,
	)

	return os.WriteFile(softRFSettingsPath, []byte(content), 0644)
}

func softRFListen() {
	for {
		if !globalSettings.SoftRFEnabled {
			time.Sleep(1 * time.Second)
			continue
		}

		if _, err := os.Stat(softRFBinaryPath); os.IsNotExist(err) {
			log.Printf("SoftRF HAT: binary not found at %s, waiting...", softRFBinaryPath)
			time.Sleep(30 * time.Second)
			continue
		}

		if err := writeSoftRFSettings(); err != nil {
			log.Printf("SoftRF HAT: failed to write settings: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("SoftRF HAT: starting subprocess")
		cmd := exec.Command(softRFBinaryPath)

		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Printf("SoftRF HAT: stdin pipe error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("SoftRF HAT: stdout pipe error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		cmd.Stderr = nil // discard SoftRF debug/info stderr

		if err := cmd.Start(); err != nil {
			log.Printf("SoftRF HAT: failed to start: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("SoftRF HAT: subprocess started (PID %d)", cmd.Process.Pid)

		exitChan := make(chan error, 1)
		go func() { exitChan <- cmd.Wait() }()

		// Writer: forward GPS NMEA from channel to SoftRF stdin.
		go func() {
			for nmea := range softRFNMEAChan {
				if _, err := fmt.Fprint(stdin, nmea); err != nil {
					return
				}
			}
		}()

		// Reader: parse $PFLAA/$PFLAU from SoftRF stdout.
		scanDone := make(chan struct{}, 1)
		go func() {
			defer func() { scanDone <- struct{}{} }()
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "$PFL") {
					continue
				}
				// Strip checksum suffix before splitting fields.
				bare := line
				if idx := strings.LastIndex(line, "*"); idx >= 0 {
					bare = line[:idx]
				}
				fields := strings.Split(bare, ",")
				if len(fields) < 2 {
					continue
				}
				switch fields[0] {
				case "$PFLAA":
					parseFlarmPFLAA(fields)
					var m msg
					m.MessageClass = MSGCLASS_SOFTRF
					m.TimeReceived = stratuxClock.Time
					msgLogAppend(m)
				case "$PFLAU":
					parseFlarmPFLAU(fields)
				}
			}
		}()

		// Wait for subprocess exit or a settings-change restart signal.
		select {
		case err := <-exitChan:
			log.Printf("SoftRF HAT: subprocess exited: %v", err)
		case <-softRFRestartChan:
			log.Printf("SoftRF HAT: restarting subprocess for settings change")
			cmd.Process.Kill()
			<-exitChan
		}

		stdin.Close()
		<-scanDone
		time.Sleep(3 * time.Second)
	}
}
