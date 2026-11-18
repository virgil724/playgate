package rtc

import (
	"net"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// LoopbackSettingEngine returns a SettingEngine that gathers ICE candidates on
// loopback only (udp4, no mDNS). It exists for tests: binding UDP sockets on
// real interfaces makes Windows Defender Firewall prompt for every freshly
// built test binary. Production code never uses it.
func LoopbackSettingEngine() webrtc.SettingEngine {
	s := webrtc.SettingEngine{}
	applyLoopbackOnly(&s)
	return s
}

// NewLoopbackPeerConnection builds a plain (browser-stand-in) PeerConnection
// restricted to loopback. Test helper.
func NewLoopbackPeerConnection() (*webrtc.PeerConnection, error) {
	api := webrtc.NewAPI(webrtc.WithSettingEngine(LoopbackSettingEngine()))
	return api.NewPeerConnection(webrtc.Configuration{})
}

func applyLoopbackOnly(s *webrtc.SettingEngine) {
	loop := map[string]bool{}
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if i.Flags&net.FlagLoopback != 0 {
				loop[i.Name] = true
			}
		}
	}
	s.SetIncludeLoopbackCandidate(true)
	s.SetInterfaceFilter(func(name string) bool { return loop[name] })
	s.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
}
