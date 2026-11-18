package android

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const adbDevicesSample = `* daemon not running; starting now at tcp:5037
* daemon started successfully
List of devices attached
emulator-5554          device product:sdk_gphone64_arm64 model:sdk_gphone64_arm64 device:emu64a transport_id:1
R58N12ABCDE            unauthorized usb:1-1 transport_id:2
192.168.1.5:5555       offline transport_id:3
0123456789ABCDEF       no permissions (user in plugdev group; are your udev rules wrong?); see [http://developer.android.com/tools/device.html] usb:1-2 transport_id:4
adb-R5CT10ABC-xyz._adb-tls-connect._tcp device product:dm3qzcx model:SM_S918B device:dm3q transport_id:5
emulator-5556          offline transport_id:6

`

func TestParseDevices(t *testing.T) {
	ds := parseDevices(adbDevicesSample)
	if len(ds) != 6 {
		t.Fatalf("got %d devices: %+v", len(ds), ds)
	}
	if d := ds[0]; d.Serial != "emulator-5554" || !d.Online() || !d.Emulator() || d.Model != "sdk_gphone64_arm64" || d.Name != "emu64a" {
		t.Errorf("emulator %+v", d)
	}
	if ds[1].State != "unauthorized" || ds[1].Online() {
		t.Errorf("unauthorized %+v", ds[1])
	}
	if ds[2].State != "offline" {
		t.Errorf("offline %+v", ds[2])
	}
	if ds[3].State != "no permissions" || ds[3].Serial != "0123456789ABCDEF" {
		t.Errorf("no permissions %+v", ds[3])
	}
	if d := ds[4]; !d.Online() || d.Model != "SM_S918B" || d.Label() != "SM S918B · adb-R5CT10ABC-xyz._adb-tls-connect._tcp" {
		t.Errorf("wireless %+v label %q", d, d.Label())
	}
	if len(parseDevices("List of devices attached\n\n")) != 0 {
		t.Error("empty list")
	}
	if len(parseDevices("error: cannot connect to daemon\n")) != 0 {
		t.Error("error output")
	}
}

func TestChooseDevice(t *testing.T) {
	phone := Device{Serial: "R58N1", State: "device", Model: "Pixel_7"}
	phone2 := Device{Serial: "R58N2", State: "device", Model: "Mi_13"}
	emu := Device{Serial: "emulator-5554", State: "device", AVD: "Medium_Phone_API_36.1"}
	booting := Device{Serial: "emulator-5556", State: "offline"}
	unauth := Device{Serial: "R58N3", State: "unauthorized"}
	avd1, avd2 := AVD{Name: "Medium_Phone_API_36.1"}, AVD{Name: "XR_Headset"}
	type want struct {
		serial, start, err string
		wait               bool
	}
	cases := []struct {
		name string
		opt  string
		devs []Device
		avds []AVD
		want want
	}{
		{"自动：一台在线", "", []Device{phone, unauth}, []AVD{avd1}, want{serial: "R58N1"}},
		{"自动：多台在线", "", []Device{phone, phone2}, nil, want{err: selMultiple}},
		{"自动：无设备一个 AVD", "", nil, []AVD{avd1}, want{start: "Medium_Phone_API_36.1"}},
		{"自动：无设备多个 AVD", "", nil, []AVD{avd1, avd2}, want{err: selChooseAVD}},
		{"自动：什么都没有", "", nil, nil, want{err: selNoDevice}},
		{"自动：只有未授权", "", []Device{unauth}, []AVD{avd1}, want{err: selUnauthorized}},
		{"自动：模拟器开机中", "", []Device{booting}, []AVD{avd1, avd2}, want{serial: "emulator-5556", wait: true}},
		{"指定序列号", "R58N2", []Device{phone, phone2}, nil, want{serial: "R58N2"}},
		{"指定序列号未授权", "R58N3", []Device{phone, unauth}, nil, want{err: selUnauthorized}},
		{"指定序列号不在", "R58N9", []Device{phone}, nil, want{err: selNotFound}},
		{"指定 AVD 已在运行", "avd:Medium_Phone_API_36.1", []Device{phone, emu}, []AVD{avd1}, want{serial: "emulator-5554"}},
		{"指定 AVD 未运行", "avd:XR_Headset", []Device{phone, emu}, []AVD{avd1, avd2}, want{start: "XR_Headset"}},
		{"指定 AVD 不存在", "avd:Nope", nil, []AVD{avd1}, want{err: selAVDNotFound}},
	}
	for _, c := range cases {
		sel, err := chooseDevice(c.opt, c.devs, c.avds)
		got := want{}
		if err != nil {
			got.err = err.Kind
		} else {
			got.serial, got.start, got.wait = sel.Serial, sel.StartAVD, sel.WaitBoot
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestListAVDs(t *testing.T) {
	dir := t.TempDir()
	avdDir := filepath.Join(dir, "Pixel_8.avd")
	_ = os.MkdirAll(avdDir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "Pixel_8.ini"), []byte("avd.ini.encoding=UTF-8\npath="+avdDir+"\npath.rel=avd/Pixel_8.avd\ntarget=android-35\n"), 0o644)
	_ = os.WriteFile(filepath.Join(avdDir, "config.ini"), []byte("avd.ini.displayname=Pixel 8 API 35\n"), 0o644)
	// path 指向不存在的目录、也没有同名 .avd：忽略
	_ = os.WriteFile(filepath.Join(dir, "Gone.ini"), []byte("path=/nonexistent/Gone.avd\n"), 0o644)
	got := listAVDs(dir)
	if len(got) != 1 || got[0].Name != "Pixel_8" || got[0].Display != "Pixel 8 API 35" || got[0].Target != "android-35" {
		t.Fatalf("listAVDs = %+v", got)
	}
	if avdHome(map[string]string{"ANDROID_AVD_HOME": "/x/avd"}) != "/x/avd" || avdHome(map[string]string{"ANDROID_AVD_HOME": "", "ANDROID_USER_HOME": "/u"}) != "/u/avd" {
		t.Error("avdHome")
	}
}

func TestDeviceOptions(t *testing.T) {
	ctx := &sdk.Context{}
	devs := []Device{{Serial: "emulator-5554", State: "device", AVD: "Medium"}, {Serial: "R1", State: "device", Model: "Pixel_7"}, {Serial: "R2", State: "unauthorized"}}
	opts := deviceOptions(ctx, devs, []AVD{{Name: "Medium"}, {Name: "XR", Display: "XR Headset"}})
	var vals []string
	for _, o := range opts {
		vals = append(vals, o.Value)
	}
	// 运行中的模拟器以 avd:名字 出现一次，不重复列出
	if !reflect.DeepEqual(vals, []string{"", "avd:Medium", "R1", "R2", "avd:XR"}) {
		t.Fatalf("options %v", vals)
	}
}
