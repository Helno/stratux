package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stratux/serial"
	"github.com/stratux/stratux/common"
)

/*
	m1421.go: ADSBee 1421 dual-band (1090ES + 978 UAT) receiver, connected over USB through the
	ADSBee 1421 Programmer (a USB CDC serial bridge, udev symlink /dev/adsbee1421).

	The module's console runs at a fixed 1,000,000 baud (changing it can break the programmer bridge).
	We select the AIRCRAFT_JSON_UAT_UPLINK reporting protocol (firmware 0.3.11-rc6 or newer), which emits
	one JSON object per line:
		{"hex":"a63993","type":"adsb_icao","link":"1090","lat":..,"lon":..,"alt_baro":..,...}   tracked aircraft
		{"uat_uplink":"<576 base64 chars>","rssi":-80,"t":123456}                               978 uplink (FIS-B)
	Aircraft are already decoded by the module. Uplinks are the de-interleaved, error-corrected 432 byte
	payload, exactly what Stratux expects from dump978.
*/

const (
	m1421Device      = "/dev/adsbee1421"
	m1421DeviceGlob  = "/dev/serial/by-id/usb-ADSBee_ADSBee_1421_Programmer_*"
	m1421Baud        = 1000000
	m1421KeepAlive   = 30 * time.Second
	m1421RSSIUnknown = -32768
)

var m1421Commands = []string{
	"AT+LOG_LEVEL=SILENT",
	"AT+PROTOCOL_OUT=CONSOLE,AIRCRAFT_JSON_UAT_UPLINK",
}

type m1421Message struct {
	UATUplink string `json:"uat_uplink"`

	Hex         string          `json:"hex"`
	Type        string          `json:"type"`
	Link        string          `json:"link"`
	Flight      *string         `json:"flight"`
	Squawk      *string         `json:"squawk"`
	Category    *string         `json:"category"`
	Lat         *float64        `json:"lat"`
	Lon         *float64        `json:"lon"`
	AltBaro     json.RawMessage `json:"alt_baro"`
	AltGeom     *int            `json:"alt_geom"`
	GS          *float64        `json:"gs"`
	Track       *float64        `json:"track"`
	TrueHeading *float64        `json:"true_heading"`
	BaroRate    *int            `json:"baro_rate"`
	GeomRate    *int            `json:"geom_rate"`
	NIC         *int            `json:"nic"`
	NACp        *int            `json:"nac_p"`
	Emergency   *string         `json:"emergency"`
	OnGround    *int            `json:"on_ground"`
	RSSI        *int            `json:"rssi"`
	SeenPos     *float64        `json:"seen_pos"`
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
			path := m1421FindDevice()
			if path == "" {
				continue
			}

			port, err := openM1421(path)
			if err != nil {
				log.Printf("M1421: %s\n", err.Error())
				continue
			}

			log.Printf("M1421 connected on %s\n", path)
			globalStatus.M1421_connected = true
			go m1421Run(port)
		}
	}()

	return nil
}

// m1421FindDevice returns the udev symlink, or the by-id path if the symlink was not created.
func m1421FindDevice() string {
	if _, err := os.Stat(m1421Device); err == nil {
		return m1421Device
	}
	if matches, _ := filepath.Glob(m1421DeviceGlob); len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func openM1421(path string) (*serial.Port, error) {
	port, err := serial.OpenPort(&serial.Config{Name: path, Baud: m1421Baud})
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
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

func m1421Run(port *serial.Port) {
	defer func() {
		globalStatus.M1421_connected = false
		port.Close()
		log.Printf("M1421 disconnected\n")
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
	lines := 0
	for scanner.Scan() && globalSettings.M1421_Enabled {
		line := strings.TrimSpace(scanner.Text())
		if lines < 5 && len(line) > 0 {
			lines++
			if len(line) > 200 {
				line = line[:200]
			}
			log.Printf("M1421 rx: %s\n", line)
		}
		m1421HandleLine(strings.TrimSpace(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		log.Printf("M1421 serial port error: %s\n", err.Error())
	}
}

// m1421HandleLine processes one console line; command replies and anything that isn't JSON are ignored.
func m1421HandleLine(line string) {
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var m m1421Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		log.Printf("M1421: bad JSON line: %s\n", err.Error())
		return
	}

	if m.UATUplink != "" {
		m1421HandleUplink(m.UATUplink)
	} else if m.Hex != "" {
		m1421HandleAircraft(&m)
	}
}

func m1421HandleUplink(b64 string) {
	frame, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(frame) != UPLINK_FRAME_DATA_BYTES {
		return
	}
	if o, msgtype := parseInput("+" + hex.EncodeToString(frame) + ";"); o != nil && msgtype != 0 {
		relayMessage(msgtype, o)
	}
}

func m1421HandleAircraft(m *m1421Message) {
	hexStr := strings.TrimPrefix(m.Hex, "~")
	addr, err := strconv.ParseUint(hexStr, 16, 32)
	if err != nil || addr > 0xFFFFFF {
		return
	}
	icao := uint32(addr)
	is978 := m.Link == "978"
	if is978 {
		globalStatus.UAT_messages_total++
	} else {
		globalStatus.ES_messages_total++
	}

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	ti, ok := traffic[icao]
	if !ok {
		ti.Last_seen = stratuxClock.Time
		ti.Last_alt = stratuxClock.Time
		ti.Icao_addr = icao
		if reg, valid := icao2reg(icao); valid {
			ti.Reg = reg
			ti.Tail = reg
		}
	}
	m1421UpdateTraffic(&ti, m, is978)

	postProcessTraffic(&ti)
	traffic[icao] = ti
	registerTrafficUpdate(ti)
	seenTraffic[icao] = true
}

func m1421UpdateTraffic(ti *TrafficInfo, m *m1421Message, is978 bool) {
	now := stratuxClock.Time

	ti.TargetType, ti.Addr_type = m1421TargetType(m.Type)
	if m.Type != "mode_s" {
		if is978 {
			ti.Last_source = TRAFFIC_SOURCE_UAT
		} else {
			ti.Last_source = TRAFFIC_SOURCE_1090ES
		}
	}

	if m.RSSI != nil && *m.RSSI != m1421RSSIUnknown {
		ti.SignalLevel = float64(*m.RSSI)
	} else {
		ti.SignalLevel = -999
	}

	onGround := m.OnGround != nil && *m.OnGround == 1

	// alt_baro is an integer, or the string "ground" in readsb output.
	baroValid := false
	var baro int
	if len(m.AltBaro) > 0 {
		if err := json.Unmarshal(m.AltBaro, &baro); err == nil {
			baroValid = true
		} else if string(m.AltBaro) == `"ground"` {
			onGround = true
		}
	}
	if baroValid {
		ti.Alt = int32(baro)
		ti.AltIsGNSS = false
		ti.Last_alt = now
		if m.AltGeom != nil {
			ti.GnssDiffFromBaroAlt = int32(*m.AltGeom - baro)
			ti.Last_GnssDiff = now
			ti.Last_GnssDiffAlt = ti.Alt
		}
	} else if m.AltGeom != nil {
		ti.Alt = int32(*m.AltGeom)
		ti.AltIsGNSS = true
		ti.Last_alt = now
	}
	ti.OnGround = onGround

	if m.Lat != nil && m.Lon != nil && (*m.Lat != 0 || *m.Lon != 0) {
		ti.Lat = float32(*m.Lat)
		ti.Lng = float32(*m.Lon)
		ti.Position_valid = true
		if isGPSValid() {
			ti.Distance, ti.Bearing = common.Distance(float64(mySituation.GPSLatitude), float64(mySituation.GPSLongitude), float64(ti.Lat), float64(ti.Lng))
			ti.BearingDist_valid = true
		}
		// The module repeats the last known position with every update, so only a recent one counts as seen.
		if m.SeenPos == nil || *m.SeenPos <= 2 {
			ti.ExtrapolatedPosition = false
			ti.Last_seen = now
		}
	} else if !ti.Position_valid {
		ti.Last_seen = ti.Last_alt
	}

	track := m.Track
	if track == nil {
		track = m.TrueHeading
	}
	if m.GS != nil && track != nil {
		ti.Speed = uint16(*m.GS)
		ti.Track = float32(*track)
		ti.Speed_valid = true
		ti.Last_speed = now
	}
	if m.BaroRate != nil {
		ti.Vvel = int16(*m.BaroRate)
	} else if m.GeomRate != nil {
		ti.Vvel = int16(*m.GeomRate)
	}

	if m.NIC != nil {
		ti.NIC = *m.NIC
	}
	if m.NACp != nil {
		ti.NACp = *m.NACp
	}
	if m.Category != nil {
		ti.Emitter_category = m1421EmitterCategory(*m.Category)
	}
	if m.Squawk != nil {
		if sq, err := strconv.Atoi(*m.Squawk); err == nil {
			ti.Squawk = sq
		}
	}
	if m.Emergency != nil {
		ti.PriorityStatus = m1421PriorityStatus(*m.Emergency)
	}
	if m.Flight != nil {
		if tail := strings.TrimSpace(*m.Flight); tail != "" {
			ti.Tail = tail
		}
	}
	if globalSettings.DisplayTrafficSource {
		ti.Tail = m1421TailWithSource(ti.Tail, ti.TargetType, is978)
	}

	ti.Timestamp = time.Now()
}

// m1421TargetType maps the module's aircraft type to a Stratux target type and a GDL90 address type.
func m1421TargetType(t string) (uint8, uint8) {
	switch t {
	case "adsb_icao", "adsb_icao_nt":
		return TARGET_TYPE_ADSB, 0
	case "adsb_other":
		return TARGET_TYPE_ADSB, 1
	case "tisb_icao":
		return TARGET_TYPE_TISB, 2
	case "tisb_trackfile", "tisb_other":
		return TARGET_TYPE_TISB, 3
	case "adsr_icao", "adsr_other":
		return TARGET_TYPE_ADSR, 6
	}
	return TARGET_TYPE_MODE_S, 0
}

// m1421EmitterCategory converts readsb "A0".."D7" to the GDL90 emitter category (A7 = 0x07, B0 = 0x08, ...).
func m1421EmitterCategory(c string) uint8 {
	if len(c) != 2 || c[0] < 'A' || c[0] > 'D' || c[1] < '0' || c[1] > '7' {
		return 0
	}
	return (c[0]-'A')*8 + (c[1] - '0')
}

// m1421PriorityStatus converts the UAT emergency string to the GDL90 emergency/priority code.
func m1421PriorityStatus(e string) uint8 {
	switch e {
	case "general":
		return 1
	case "lifeguard":
		return 2
	case "minfuel":
		return 3
	case "nordo":
		return 4
	case "unlawful":
		return 5
	case "downed":
		return 6
	}
	return 0
}

// m1421TailWithSource prefixes the callsign with the source (e = 1090ES, u = UAT) and target type, as the other receivers do.
func m1421TailWithSource(tail string, targetType uint8, is978 bool) string {
	prefix := "e"
	if is978 {
		prefix = "u"
	}
	typeCode := " "
	switch targetType {
	case TARGET_TYPE_ADSB:
		typeCode = "a"
	case TARGET_TYPE_ADSR:
		typeCode = "r"
	case TARGET_TYPE_TISB:
		typeCode = "t"
	}
	switch {
	case len(tail) == 0:
		return prefix + typeCode
	case len(tail) < 7 && tail[0] != 'e' && tail[0] != 'u':
		return prefix + typeCode + tail
	case len(tail) == 7 && tail[0] != 'e' && tail[0] != 'u':
		return prefix + typeCode + tail[1:]
	case len(tail) > 1:
		return prefix + typeCode + tail[2:]
	}
	return tail
}
