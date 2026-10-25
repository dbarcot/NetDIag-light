package sysapi

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
)

// Band is a Wi-Fi frequency band; "" when unknown.
type Band string

const (
	Band24GHz Band = "2.4GHz"
	Band5GHz  Band = "5GHz"
	Band6GHz  Band = "6GHz"
)

// WiFiStatus is the state of one WLAN interface. Optional numbers are nil
// when Windows did not report them.
type WiFiStatus struct {
	InterfaceGUID string // "{...}", matches Adapter.GUID
	Description   string
	Connected     bool
	SSID          string
	// SSIDHidden is set when Windows denied the connection query because
	// Location Services are off for desktop apps (Windows 11 24H2+). Not an error.
	SSIDHidden   bool
	BSSID        net.HardwareAddr
	SignalPct    *int
	RSSI         *int // dBm
	Channel      *int
	FrequencyMHz *int
	Band         Band
	BandSource   BandSource
}

// BandSource says how certain Band is.
type BandSource string

const (
	BandFromFrequency BandSource = "frequency" // measured center frequency: certain
	BandFromChannelNo BandSource = "channel"   // channel number only: 6 GHz low channels read as 2.4 GHz
)

// Is24GHz reports a certain 2.4 GHz link; WIFI_24GHZ is raised only then.
func (s WiFiStatus) Is24GHz() bool {
	return s.Band == Band24GHz && s.BandSource == BandFromFrequency
}

// BandLabel is the band for display, with "?" when it only comes from the
// channel number (e.g. "2.4 GHz?"); "" when unknown.
func (s WiFiStatus) BandLabel() string {
	var l string
	switch s.Band {
	case Band24GHz:
		l = "2.4 GHz"
	case Band5GHz:
		l = "5 GHz"
	case Band6GHz:
		l = "6 GHz"
	default:
		return ""
	}
	if s.BandSource != BandFromFrequency {
		l += "?"
	}
	return l
}

// HasDetails reports whether any connection detail is known; if not, the
// UI shows "Wi-Fi: details unavailable" and Wi-Fi flags are skipped.
func (s WiFiStatus) HasDetails() bool {
	return s.SSID != "" || s.SignalPct != nil || s.RSSI != nil || s.Channel != nil || s.Band != ""
}

// BandFromFrequencyMHz maps a channel center frequency to its band.
func BandFromFrequencyMHz(mhz int) Band {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return Band24GHz
	case mhz >= 4900 && mhz < 5925:
		return Band5GHz
	case mhz >= 5925 && mhz <= 7125:
		return Band6GHz
	default:
		return ""
	}
}

// BandFromChannel is the fallback when the frequency is unknown. 6 GHz
// channel numbers overlap 2.4 GHz ones, so a 6 GHz link on channel 1–14
// would read as 2.4 GHz here; the frequency is used whenever available.
func BandFromChannel(ch int) Band {
	switch {
	case ch >= 1 && ch <= 14:
		return Band24GHz
	case ch >= 32 && ch <= 177:
		return Band5GHz
	default:
		return ""
	}
}

// ChannelFromFrequencyMHz returns the channel number of a center frequency
// within its band, or 0.
func ChannelFromFrequencyMHz(mhz int) int {
	switch BandFromFrequencyMHz(mhz) {
	case Band24GHz:
		if mhz == 2484 {
			return 14
		}
		return (mhz - 2407) / 5
	case Band5GHz:
		return (mhz - 5000) / 5
	case Band6GHz:
		if mhz == 5935 {
			return 2
		}
		return (mhz - 5950) / 5
	default:
		return 0
	}
}

// matchFrequency finds the connected BSS's frequency in the BSS list: by
// BSSID when known, else by channel number. A channel match is used only
// when every BSS on that channel number has the same frequency (channel 1
// exists in both 2.4 and 6 GHz).
func matchFrequency(list []wlanBSS, bssid net.HardwareAddr, channel int) (int, bool) {
	if len(bssid) > 0 {
		for _, b := range list {
			if b.FrequencyMHz > 0 && bytes.Equal(b.BSSID, bssid) {
				return b.FrequencyMHz, true
			}
		}
		return 0, false
	}
	if channel <= 0 {
		return 0, false
	}
	freq := 0
	for _, b := range list {
		if ChannelFromFrequencyMHz(b.FrequencyMHz) != channel {
			continue
		}
		if freq != 0 && freq != b.FrequencyMHz {
			return 0, false // ambiguous
		}
		freq = b.FrequencyMHz
	}
	return freq, freq != 0
}

// SignalFromRSSI is Windows' own linear mapping: -100 dBm → 0 %, -50 dBm → 100 %.
func SignalFromRSSI(dbm int) int {
	return min(max(2*(dbm+100), 0), 100)
}

// errWLANUnavailable means there is no WLAN service or DLL: a clean
// "no Wi-Fi" result.
var errWLANUnavailable = errors.New("WLAN API unavailable")

type wlanIface struct {
	GUID        string
	Description string
	Connected   bool
}

type wlanConn struct {
	SSID          string
	BSSID         net.HardwareAddr
	SignalQuality int
}

// wlanBSS is one entry of the cached BSS list.
type wlanBSS struct {
	BSSID        net.HardwareAddr
	FrequencyMHz int
}

// wlanAPI is the raw WLAN API, one call per query, so the assembly logic
// in WLANWiFi is tested with a fake. Errors matching fs.ErrPermission
// mean the location-privacy denial.
type wlanAPI interface {
	interfaces() ([]wlanIface, error)
	connection(guid string) (wlanConn, error)
	rssi(guid string) (int, error)
	channel(guid string) (int, error)
	bssList(guid string) ([]wlanBSS, error)
	close()
}

// WLANWiFi implements WiFi on top of a wlanAPI.
type WLANWiFi struct {
	open func() (wlanAPI, error)
}

func (w WLANWiFi) Status(ctx context.Context) ([]WiFiStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api, err := w.open()
	if errors.Is(err, errWLANUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer api.close()
	ifaces, err := api.interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]WiFiStatus, 0, len(ifaces))
	for _, i := range ifaces {
		out = append(out, queryWiFi(api, i))
	}
	return out, nil
}

// queryWiFi asks for every field separately so one denied or failing query
// never hides the others (SPEC §5 WIFI-01 note).
func queryWiFi(api wlanAPI, i wlanIface) WiFiStatus {
	st := WiFiStatus{InterfaceGUID: i.GUID, Description: i.Description, Connected: i.Connected}
	if !i.Connected {
		return st
	}
	conn, err := api.connection(i.GUID)
	switch {
	case err == nil:
		st.SSID, st.BSSID = conn.SSID, conn.BSSID
		st.SignalPct = ptr(conn.SignalQuality)
	case errors.Is(err, fs.ErrPermission):
		st.SSIDHidden = true
	}
	if r, err := api.rssi(i.GUID); err == nil {
		st.RSSI = ptr(r)
		if st.SignalPct == nil {
			st.SignalPct = ptr(SignalFromRSSI(r))
		}
	}
	if ch, err := api.channel(i.GUID); err == nil && ch > 0 {
		st.Channel = ptr(ch)
	}
	// The frequency needs the BSSID, or at least the channel when the SSID
	// query was denied.
	if len(st.BSSID) > 0 || st.Channel != nil {
		ch := 0
		if st.Channel != nil {
			ch = *st.Channel
		}
		if list, err := api.bssList(i.GUID); err == nil {
			if f, ok := matchFrequency(list, st.BSSID, ch); ok {
				st.FrequencyMHz = ptr(f)
			}
		}
	}
	switch {
	case st.FrequencyMHz != nil:
		st.Band, st.BandSource = BandFromFrequencyMHz(*st.FrequencyMHz), BandFromFrequency
	case st.Channel != nil:
		st.Band, st.BandSource = BandFromChannel(*st.Channel), BandFromChannelNo
	}
	if st.Band == "" {
		st.BandSource = ""
	}
	return st
}

func ptr[T any](v T) *T { return &v }
