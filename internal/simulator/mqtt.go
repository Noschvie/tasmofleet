package simulator

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// mqttLink is the MQTT side of one simulated device. A nil *mqttLink (HTTP
// only device) is valid: every method is a no-op.
type mqttLink struct {
	d   *simDevice
	url string

	mu     sync.Mutex
	client paho.Client
	conn   net.Conn
	muted  bool // main loop blocked: nothing is published or processed
}

func newMQTTLink(d *simDevice, brokerURL string) *mqttLink {
	if brokerURL == "" {
		return nil
	}
	return &mqttLink{d: d, url: brokerURL}
}

func (m *mqttLink) setMuted(v bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.muted = v
	m.mu.Unlock()
}

// connect (re)connects and publishes what a booting device publishes:
// retained LWT Online, retained discovery (normal firmware without
// DeviceSpec.NoDiscovery only) and INFO1-3.
func (m *mqttLink) connect() {
	if m == nil {
		return
	}
	d := m.d
	m.mu.Lock()
	m.muted = false
	m.mu.Unlock()
	opts := paho.NewClientOptions().AddBroker(m.url).SetClientID(d.topic).
		SetKeepAlive(30*time.Second).SetCleanSession(true).SetAutoReconnect(false).
		SetConnectRetry(false).SetConnectTimeout(5*time.Second).
		SetWill("tele/"+d.topic+"/LWT", "Offline", 0, true).
		SetCustomOpenConnectionFn(func(u *url.URL, _ paho.ClientOptions) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(d.ctx, "tcp", u.Host)
			if err == nil {
				m.mu.Lock()
				m.conn = c
				m.mu.Unlock()
			}
			return c, err
		})
	c := paho.NewClient(opts)
	if t := c.Connect(); !t.WaitTimeout(6*time.Second) || t.Error() != nil {
		return
	}
	m.mu.Lock()
	if m.client != nil && m.client.IsConnected() {
		m.mu.Unlock()
		c.Disconnect(0)
		return
	}
	m.client = c
	m.mu.Unlock()
	handler := func(_ paho.Client, msg paho.Message) { m.onCommand(msg) }
	c.Subscribe("cmnd/"+d.topic+"/#", 0, handler)
	c.Subscribe("cmnd/tasmotas/#", 0, handler)
	m.raw("tele/"+d.topic+"/LWT", "Online", true)
	d.mu.Lock()
	minimal := d.minimalLocked()
	v := d.viewLocked()
	image, version := d.image, d.version
	d.mu.Unlock()
	if !minimal && !d.spec.NoDiscovery {
		if cfg, sensors := discoveryFor(d.f.o.FixturesDir, d.cap); cfg != nil {
			mac := strings.ReplaceAll(d.mac, ":", "")
			m.raw("tasmota/discovery/"+mac+"/config", d.substitute(string(cfg), version), true)
			if sensors == nil {
				sensors = []byte(`{"sn":{"Time":"` + v.now.Format("2006-01-02T15:04:05") + `"},"ver":1}`)
			}
			m.raw("tasmota/discovery/"+mac+"/sensors", d.substitute(string(sensors), version), true)
		}
	}
	info1 := `{"Info1":{"Module":"Sonoff Basic","Version":"` + version + `(release-` + image + `)","FallbackTopic":"cmnd/` + d.topic + `_fb/","GroupTopic":"cmnd/tasmotas/"}}`
	m.raw("tele/"+d.topic+"/INFO1", info1, false)
	m.raw("tele/"+d.topic+"/INFO2", `{"Info2":{"WebServerMode":"Admin","Hostname":"`+d.topic+`","IPAddress":"`+d.host+`"}}`, false)
	m.raw("tele/"+d.topic+"/INFO3", `{"Info3":{"RestartReason":`+jsonString(v.reason)+`,"BootCount":`+itoa(v.bootCount)+`}}`, false)
}

func itoa(n int) string { return strconv.Itoa(n) }

func (d *simDevice) substitute(s, version string) string {
	if d.topic != d.cap.baseTopic {
		s = strings.ReplaceAll(s, d.cap.baseTopic, d.topic)
	}
	if version != d.cap.version {
		s = strings.ReplaceAll(s, d.cap.version, version)
	}
	s = strings.ReplaceAll(s, d.cap.ip, d.host)
	s = strings.ReplaceAll(s, strings.ReplaceAll(d.cap.mac, ":", ""), strings.ReplaceAll(d.mac, ":", ""))
	return s
}

func (m *mqttLink) connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client != nil && m.client.IsConnected() && !m.muted
}

func (m *mqttLink) raw(topic, payload string, retain bool) {
	m.mu.Lock()
	c := m.client
	m.mu.Unlock()
	if c == nil || !c.IsConnected() {
		return
	}
	c.Publish(topic, 0, retain, payload).WaitTimeout(2 * time.Second)
}

// publish sends unless the device's main loop is blocked.
func (m *mqttLink) publish(topic, payload string, retain bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	muted := m.muted
	m.mu.Unlock()
	if muted {
		return
	}
	m.raw(topic, payload, retain)
}

// disconnectClean ends the simulated device for good (Close): DISCONNECT
// sent, so the broker does not publish a will. Tasmota's own restart is not
// this: see reboot.
func (m *mqttLink) disconnectClean() {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.client
	m.client = nil
	m.mu.Unlock()
	if c != nil {
		c.Disconnect(100)
	}
}

// dropUnclean kills the TCP connection: the broker fires the retained will.
func (m *mqttLink) dropUnclean() {
	if m == nil {
		return
	}
	m.mu.Lock()
	conn := m.conn
	c := m.client
	m.client = nil
	m.mu.Unlock()
	if conn != nil {
		_ = conn.Close() // the point is the abrupt close
	}
	if c != nil {
		c.Disconnect(0)
	}
}

// startBlockTimer models the broker's keepalive timeout during a long
// blocking download: after d the connection is dropped uncleanly.
func (m *mqttLink) startBlockTimer(after time.Duration) (stop func()) {
	if m == nil {
		return func() {}
	}
	t := time.AfterFunc(after, m.dropUnclean)
	return func() { t.Stop() }
}

func (m *mqttLink) onCommand(msg paho.Message) {
	m.mu.Lock()
	muted := m.muted
	m.mu.Unlock()
	if muted {
		return
	}
	d := m.d
	parts := strings.SplitN(msg.Topic(), "/", 3)
	if len(parts) < 3 {
		return
	}
	// PubSubClient::readPacket closes the connection (no DISCONNECT, so the will fires)
	// for a packet of MQTT_MAX_PACKET_SIZE bytes or more.
	if packetSize(msg) >= mqttMaxPacket {
		d.spawn(func() {
			m.dropUnclean()
			if d.sleep(secMQTTRetry) {
				m.connect()
			}
		})
		return
	}
	cmd := strings.TrimSpace(parts[2] + " " + string(msg.Payload()))
	d.mu.Lock()
	d.cmds = append(d.cmds, redactFileCmd(cmd))
	d.mu.Unlock()
	_, out := d.execMQTT(cmd, len(msg.Topic()), parts[1] == "tasmotas")
	d.publishAll(out)
}

// packetSize is the size on the wire of the PUBLISH the device received.
func packetSize(msg paho.Message) int {
	rl := 2 + len(msg.Topic()) + len(msg.Payload())
	if msg.Qos() > 0 {
		rl += 2
	}
	llen := 1
	for n := rl; n >= 128; n >>= 7 {
		llen++
	}
	return 1 + llen + rl
}

// redactFileCmd keeps the credentials of FileDownload/FileUpload out of the
// recorded commands: the payload carries the device's MQTT password.
func redactFileCmd(cmd string) string {
	name, data, _ := strings.Cut(cmd, " ")
	switch strings.ToLower(name) {
	case "filedownload", "fileupload":
		if len(data) > 8 {
			return name + " <" + itoa(len(data)) + " bytes>"
		}
	case "backlog", "backlog0", "backlog2", "backlog3":
		segs := strings.Split(data, ";")
		for i, s := range segs {
			if r := redactFileCmd(strings.TrimSpace(s)); r != strings.TrimSpace(s) {
				segs[i] = r // only a redacted segment changes; the others stay as sent
			}
		}
		return name + " " + strings.Join(segs, ";")
	}
	return cmd
}

// teleLoop publishes STATE every TelePeriod while the device is running.
func (d *simDevice) teleLoop(period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		run := d.phase == PhaseRun || d.phase == PhasePending
		v := d.viewLocked()
		d.mu.Unlock()
		if run && d.mq.connected() {
			d.mq.publish("tele/"+d.topic+"/STATE", string(d.render("state.json", v)), false)
			if p := d.sensorPayload(); p != "" {
				d.mq.publish("tele/"+d.topic+"/SENSOR", p, false)
			}
		}
	}
}

// flapLoop toggles the connection while a "flap" fault is present: LWT
// Offline (broker, unclean drop) followed by Online (reconnect).
func (d *simDevice) flapLoop() {
	for {
		d.mu.Lock()
		var gap time.Duration
		active := false
		for _, f := range d.faults {
			if f.Kind == FaultFlap {
				active, gap = true, f.Duration
			}
		}
		run := d.phase == PhaseRun
		d.mu.Unlock()
		if gap <= 0 {
			gap = 200 * time.Millisecond
		}
		if !d.sleepFor(gap) {
			return
		}
		if !active {
			d.mu.Lock()
			d.flapping = false
			d.mu.Unlock()
			return
		}
		if run && d.mq.connected() {
			d.mq.dropUnclean()
			d.mu.Lock()
			d.mqttCount++
			d.mu.Unlock()
			if !d.sleepFor(gap) {
				return
			}
			d.mq.connect()
		}
	}
}
