package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var vaapiProbe struct {
	once      sync.Once
	device    string
	available bool
}

// VAAPIDevice returns the first DRM render node that FFmpeg can use for VA-API.
func VAAPIDevice() (string, bool) {
	vaapiProbe.once.Do(func() {
		devices, err := filepath.Glob("/dev/dri/renderD*")
		if err != nil || len(devices) == 0 {
			return
		}
		if _, err = exec.LookPath("ffmpeg"); err != nil {
			return
		}
		for _, device := range devices {
			if err = probeVAAPI(device); err == nil {
				vaapiProbe.device = device
				vaapiProbe.available = true
				return
			}
		}
	})
	return vaapiProbe.device, vaapiProbe.available
}

func probeVAAPI(device string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "vaapi=va:" + device,
		"-filter_hw_device", "va",
		"-f", "lavfi", "-i", "color=c=black:s=128x128:r=1",
		"-frames:v", "1", "-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi", "-qp", "20", "-f", "null", "-",
	}
	return exec.CommandContext(ctx, "ffmpeg", args...).Run()
}

func VAAPIArgs(device string) []string {
	return []string{"-init_hw_device", "vaapi=va:" + device, "-filter_hw_device", "va"}
}

func FFmpegEnv(home string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	for _, name := range []string{"LIBVA_DRIVER_NAME", "LIBVA_DRIVERS_PATH"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func VAAPIEnabled(mode string) bool {
	if mode == "off" {
		return false
	}
	_, available := VAAPIDevice()
	return available
}
