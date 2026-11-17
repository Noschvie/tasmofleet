package simulator

import (
	"bytes"
	"crypto/md5" //nolint:gosec // the protocol mandates MD5
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
)

// type8Device is an ESP32 on the embedded broker with the default files; ufsMQTT is the build option
// USE_UFILESYS_MQTT (Tasmota arendst/Tasmota#25278), which stock builds leave out.
func type8Device(t *testing.T, name string, ufsMQTT bool) *fileBus {
	t.Helper()
	b, c, f := newBrokerFleet(t, Extra{})
	add(t, f, DeviceSpec{Name: name, Profile: ProfileESP32, Version: "16.0.0", MQTTPassword: testMQTTPassword, UfsMQTT: ufsMQTT})
	c.waitFor(t, "[R]tele/"+name+"/LWT=Online")
	return newFileBus(t, b, name)
}

func type8Start(pw, file string) string {
	return fmt.Sprintf(`{"Password":%q,"Type":8,"File":%q}`, pw, file)
}

// Why: TasmoFleet decides from the device's own answer whether its files can come over MQTT, so the
// simulator must give both answers the way the firmware does (xdrv_02_1_mqtt_file.ino,
// MqttFileDownloadValidate): the password is checked first (Error 1) on every build; a stock build then
// answers Error 3 for Type 8, a build with USE_UFILESYS_MQTT sends the file, names it in the header
// without its leading slash, and answers Error 2 for a missing file or a folder. A simulator that
// answered otherwise would let the backup's fallback and its header check pass against a device that
// does not exist.
func TestSimType8BothAnswers(t *testing.T) {
	t.Parallel()
	stock := type8Device(t, "stock", false)
	stock.send("filedownload", type8Start("wrong", "/autoexec.be"))
	if got := next(t, stock.down); got != `{"FileDownload":"Error 1"}` {
		t.Fatalf("stock, wrong password: %s", got)
	}
	stock.send("filedownload", type8Start(testMQTTPassword, "/autoexec.be"))
	if got := next(t, stock.down); got != `{"FileDownload":"Error 3"}` {
		t.Fatalf("stock, Type 8: %s", got)
	}
	// The stock device is idle again: a settings dump still works right after.
	if data, _, _ := stock.download(testMQTTPassword); len(data) == 0 {
		t.Fatal("stock: the dump after Error 3 is empty")
	}

	ufs := type8Device(t, "ufs", true)
	ufs.send("filedownload", type8Start("wrong", "/autoexec.be"))
	if got := next(t, ufs.down); got != `{"FileDownload":"Error 1"}` {
		t.Fatalf("ufs, wrong password: %s", got)
	}
	want := []byte("print('autoexec')\n")
	for _, name := range []string{"/autoexec.be", "autoexec.be"} { // MqttFileUfsName adds the slash
		ufs.send("filedownload", type8Start(testMQTTPassword, name))
		if got := next(t, ufs.down); got != `{"FileDownload":"Started"}` {
			t.Fatalf("ufs %s: %s", name, got)
		}
		hdr := next(t, ufs.down)
		if f := jsonField(t, hdr, "File"); f != "autoexec.be" {
			t.Fatalf("header File %v: the firmware names the file without its leading slash", f)
		}
		if typ := jsonField(t, hdr, "Type"); typ != float64(8) {
			t.Fatalf("header Type %v", typ)
		}
		if size := jsonField(t, hdr, "Size"); size != float64(len(want)) {
			t.Fatalf("header Size %v", size)
		}
		ufs.send("filedownload", "?")
		raw, err := base64.StdEncoding.DecodeString(jsonField(t, next(t, ufs.down), "Data").(string))
		if err != nil || !bytes.Equal(raw, want) {
			t.Fatalf("data %q, %v", raw, err)
		}
		ufs.send("filedownload", "?")
		sum := md5.Sum(want) //nolint:gosec // see import
		if md := jsonField(t, next(t, ufs.down), "Md5"); md != hex.EncodeToString(sum[:]) {
			t.Fatalf("md5 %v", md)
		}
		if got := next(t, ufs.down); got != `{"FileDownload":"Done"}` {
			t.Fatalf("after Md5: %s", got)
		}
	}
	for _, missing := range []string{"/nope.be", "/lib"} { // not there, a folder
		ufs.send("filedownload", type8Start(testMQTTPassword, missing))
		if got := next(t, ufs.down); got != `{"FileDownload":"Error 2"}` {
			t.Fatalf("ufs %s: %s", missing, got)
		}
	}
}
