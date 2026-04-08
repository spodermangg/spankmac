// spankmac detects slaps/hits on the laptop and plays audio responses.
// It reads the Apple Silicon accelerometer directly via IOKit HID —
// no separate sensor daemon required. Needs sudo.
// With help from https://github.com/taigrr/spank
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
)

var version = "dev"

//go:embed audio/pain/*.mp3
var painAudio embed.FS

//go:embed audio/sexy/*.mp3
var sexyAudio embed.FS

//go:embed audio/halo/*.mp3
var haloAudio embed.FS

//go:embed audio/lizard/*.mp3
var lizardAudio embed.FS

//go:embed audio/halflife2/*.mp3
var halflife2Audio embed.FS

//go:embed gui/*
var guiFS embed.FS

//go:embed assets/*
var assetsFS embed.FS

var (
	sexyMode     bool
	haloMode     bool
	lizardMode   bool
	halflife2Mode  bool
	customPath   string
	customFiles  []string
	fastMode     bool
	minAmplitude float64
	cooldownMs   int
	stdioMode      bool
	volumeScaling  bool
	paused         bool
	pausedMu       sync.RWMutex
	speedRatio     float64
	guiMode        bool
	nativeMode     bool
	packUpdates    = make(chan string, 5)
	activePackName = "pain"
)

// sensorReady is closed once shared memory is created and the sensor
// worker is about to enter the CFRunLoop.
var sensorReady = make(chan struct{})

// sensorErr receives any error from the sensor worker.
var sensorErr = make(chan error, 1)

type GUIEvent struct {
	Timestamp  string  `json:"timestamp"`
	SlapNumber int     `json:"slapNumber"`
	Amplitude  float64 `json:"amplitude"`
	Severity   string  `json:"severity"`
	File       string  `json:"file"`
}

var guiEvents = make(chan GUIEvent, 100)
var guiClients sync.Map

type playMode int

const (
	modeRandom playMode = iota
	modeEscalation
)

const (
	// decayHalfLife is how many seconds of inactivity before intensity
	// halves. Controls how fast escalation fades.
	decayHalfLife = 30.0

	// defaultMinAmplitude is the default detection threshold.
	defaultMinAmplitude = 0.05

	// defaultCooldownMs is the default cooldown between audio responses.
	defaultCooldownMs = 750

	// defaultSpeedRatio is the default playback speed (1.0 = normal).
	defaultSpeedRatio = 1.0

	// defaultSensorPollInterval is how often we check for new accelerometer data.
	defaultSensorPollInterval = 10 * time.Millisecond

	// defaultMaxSampleBatch caps the number of accelerometer samples processed
	// per tick to avoid falling behind.
	defaultMaxSampleBatch = 200

	// sensorStartupDelay gives the sensor time to start producing data.
	sensorStartupDelay = 100 * time.Millisecond
)

type runtimeTuning struct {
	minAmplitude float64
	cooldown     time.Duration
	pollInterval time.Duration
	maxBatch     int
}

func defaultTuning() runtimeTuning {
	return runtimeTuning{
		minAmplitude: defaultMinAmplitude,
		cooldown:     time.Duration(defaultCooldownMs) * time.Millisecond,
		pollInterval: defaultSensorPollInterval,
		maxBatch:     defaultMaxSampleBatch,
	}
}

func applyFastOverlay(base runtimeTuning) runtimeTuning {
	base.pollInterval = 4 * time.Millisecond
	base.cooldown = 350 * time.Millisecond
	if base.minAmplitude > 0.18 {
		base.minAmplitude = 0.18
	}
	if base.maxBatch < 320 {
		base.maxBatch = 320
	}
	return base
}

type soundPack struct {
	name   string
	fs     embed.FS
	dir    string
	mode   playMode
	files  []string
	custom bool
}

func (sp *soundPack) loadFiles() error {
	if sp.custom {
		entries, err := os.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	} else {
		entries, err := sp.fs.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	}
	sort.Strings(sp.files)
	if len(sp.files) == 0 {
		return fmt.Errorf("no audio files found in %s", sp.dir)
	}
	return nil
}

type slapTracker struct {
	mu       sync.Mutex
	score    float64
	lastTime time.Time
	total    int
	halfLife float64 // seconds
	scale    float64 // controls the escalation curve shape
	pack     *soundPack
	sens     float64 // sensitivity multiplier
}

func newSlapTracker(pack *soundPack, cooldown time.Duration) *slapTracker {
	// scale maps the exponential curve so that sustained max-rate
	// slapping (one per cooldown) reaches the final file. At steady
	// state the score converges to ssMax; we set scale so that score
	// maps to the last index.
	cooldownSec := cooldown.Seconds()
	ssMax := 1.0 / (1.0 - math.Pow(0.5, cooldownSec/decayHalfLife))
	scale := (ssMax - 1) / math.Log(float64(len(pack.files)+1))

	sens := 1.0
	if pack.name == "halflife2" {
		sens = 3.0 // Much more aggressive escalation for HL2
	}

	return &slapTracker{
		halfLife: decayHalfLife,
		scale:    scale,
		pack:     pack,
		sens:     sens,
	}
}

func (st *slapTracker) record(now time.Time) (int, float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.lastTime.IsZero() {
		elapsed := now.Sub(st.lastTime).Seconds()
		st.score *= math.Pow(0.5, elapsed/st.halfLife)
	}
	st.score += 1.0 * st.sens
	st.lastTime = now
	st.total++
	return st.total, st.score
}

func (st *slapTracker) getFile(score float64) string {
	if st.pack.mode == modeRandom {
		return st.pack.files[rand.Intn(len(st.pack.files))]
	}

	// Escalation: 1-exp(-x) curve maps score to file index.
	// At sustained max slap rate, score reaches ssMax which maps
	// to the final file.
	maxIdx := len(st.pack.files) - 1
	idx := min(int(float64(len(st.pack.files)) * (1.0 - math.Exp(-(score-1)/st.scale))), maxIdx)
	return st.pack.files[idx]
}

func main() {
	cmd := &cobra.Command{
		Use:   "spankmac",
		Short: "Yells 'ow!' when you slap the laptop",
		Long: `spankmac reads the Apple Silicon accelerometer directly via IOKit HID
and plays audio responses when a slap or hit is detected.

Requires sudo (for IOKit HID access to the accelerometer).

Use --sexy for a different experience. In sexy mode, the more you slap
within a minute, the more intense the sounds become.

Use --halo to play random audio clips from Halo soundtracks on each slap.

Use --lizard for lizard mode. Like sexy mode, the more you slap
within a minute, the more intense the sounds become.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			// Explicit flags override fast preset defaults
			if cmd.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if cmd.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			return run(cmd.Context(), tuning)
		},
		SilenceUsage: true,
	}

	cmd.PersistentFlags().BoolVarP(&sexyMode, "sexy", "s", false, "Enable sexy mode")
	cmd.PersistentFlags().BoolVarP(&haloMode, "halo", "H", false, "Enable halo mode")
	cmd.PersistentFlags().BoolVarP(&lizardMode, "lizard", "l", false, "Enable lizard mode (escalating intensity)")
	cmd.PersistentFlags().BoolVarP(&halflife2Mode, "halflife2", "j", false, "Enable Half-Life 2 mode (random)")
	cmd.PersistentFlags().StringVarP(&customPath, "custom", "c", "", "Path to custom MP3 audio directory")
	cmd.PersistentFlags().BoolVar(&fastMode, "fast", false, "Enable faster detection tuning (shorter cooldown, higher sensitivity)")
	cmd.PersistentFlags().StringSliceVar(&customFiles, "custom-files", nil, "Comma-separated list of custom MP3 files")
	cmd.PersistentFlags().Float64Var(&minAmplitude, "min-amplitude", defaultMinAmplitude, "Minimum amplitude threshold (0.0-1.0, lower = more sensitive)")
	cmd.PersistentFlags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Cooldown between responses in milliseconds")
	cmd.PersistentFlags().BoolVar(&stdioMode, "stdio", false, "Enable stdio mode: JSON output and stdin commands")
	cmd.PersistentFlags().BoolVar(&volumeScaling, "volume-scaling", false, "Scale playback volume by slap amplitude")
	cmd.PersistentFlags().Float64Var(&speedRatio, "speed", defaultSpeedRatio, "Playback speed multiplier (0.5 = half speed, 2.0 = double speed)")

	cmdGUI := &cobra.Command{
		Use:   "gui",
		Short: "Launch the graphical user interface",
		RunE: func(c *cobra.Command, args []string) error {
			guiMode = true
			if os.Geteuid() != 0 {
				path, err := os.Executable()
				if err != nil {
					return err
				}
				argsStr := "gui"
				if nativeMode {
					argsStr += " --native"
				}
				script := fmt.Sprintf(`do shell script "'%s' %s > /dev/null 2>&1 &" with administrator privileges`, path, argsStr)
				exec.Command("osascript", "-e", script).Start()
				return nil
			}
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			if c.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if c.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			go startWebServer()
			if !nativeMode {
				exec.Command("open", "http://localhost:8080").Start()
			}
			return run(c.Context(), tuning)
		},
	}
	cmdGUI.Flags().BoolVar(&nativeMode, "native", false, "Start native UI app instead of browser")
	cmd.AddCommand(cmdGUI)

	cmdInstall := &cobra.Command{
		Use:   "install-app",
		Short: "Install SpankMac.app wrapper to your Applications folder",
		RunE: func(c *cobra.Command, args []string) error {
			return installApplet()
		},
	}
	cmd.AddCommand(cmdInstall)

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, tuning runtimeTuning) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("spankmac requires root privileges for accelerometer access, run with: sudo spankmac")
	}

	modeCount := 0
	if sexyMode {
		modeCount++
	}
	if haloMode {
		modeCount++
	}
	if lizardMode {
		modeCount++
	}
	if halflife2Mode {
		modeCount++
	}
	if customPath != "" || len(customFiles) > 0 {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("--sexy, --halo, --lizard, and --custom/--custom-files are mutually exclusive; pick one")
	}

	if tuning.minAmplitude < 0 || tuning.minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}
	if tuning.cooldown <= 0 {
		return fmt.Errorf("--cooldown must be greater than 0")
	}

	var pack = loadPack(activePackName)
	if customPath != "" || len(customFiles) > 0 {
		activePackName = "custom"
		if len(customFiles) > 0 {
			pack = &soundPack{name: "custom", mode: modeRandom, custom: true, files: customFiles}
		} else {
			pack = &soundPack{name: "custom", dir: customPath, mode: modeRandom, custom: true}
		}
	} else {
		switch {
		case sexyMode:
			activePackName = "sexy"
		case haloMode:
			activePackName = "halo"
		case lizardMode:
			activePackName = "lizard"
		case halflife2Mode:
			activePackName = "halflife2"
		default:
			activePackName = "pain"
		}
		pack = loadPack(activePackName)
	}

	// Only load files if not already set (customFiles case)
	if len(pack.files) == 0 {
		if err := pack.loadFiles(); err != nil {
			return fmt.Errorf("loading %s audio: %w", pack.name, err)
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create shared memory for accelerometer data.
	accelRing, err := shm.CreateRing(shm.NameAccel)
	if err != nil {
		return fmt.Errorf("creating accel shm: %w", err)
	}
	defer accelRing.Close()
	defer accelRing.Unlink()

	// Start the sensor worker in a background goroutine.
	// sensor.Run() needs runtime.LockOSThread for CFRunLoop, which it
	// handles internally. We launch detection on the current goroutine.
	go func() {
		close(sensorReady)
		if err := sensor.Run(sensor.Config{
			AccelRing: accelRing,
			Restarts:  0,
		}); err != nil {
			sensorErr <- err
		}
	}()

	// Wait for sensor to be ready.
	select {
	case <-sensorReady:
	case err := <-sensorErr:
		return fmt.Errorf("sensor worker failed: %w", err)
	case <-ctx.Done():
		return nil
	}

	// Give the sensor a moment to start producing data.
	time.Sleep(sensorStartupDelay)

	return listenForSlaps(ctx, pack, accelRing, tuning)
}

func loadPack(name string) *soundPack {
	var p *soundPack
	switch name {
	case "sexy":
		p = &soundPack{name: "sexy", fs: sexyAudio, dir: "audio/sexy", mode: modeEscalation}
	case "halo":
		p = &soundPack{name: "halo", fs: haloAudio, dir: "audio/halo", mode: modeRandom}
	case "lizard":
		p = &soundPack{name: "lizard", fs: lizardAudio, dir: "audio/lizard", mode: modeEscalation}
	case "halflife2":
		p = &soundPack{name: "halflife2", fs: halflife2Audio, dir: "audio/halflife2", mode: modeRandom}
	default:
		p = &soundPack{name: "pain", fs: painAudio, dir: "audio/pain", mode: modeRandom}
	}
	_ = p.loadFiles()
	return p
}

func listenForSlaps(ctx context.Context, pack *soundPack, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	tracker := newSlapTracker(pack, tuning.cooldown)
	speakerInit := false
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastYell time.Time

	// Start stdin command reader if in JSON mode
	if stdioMode {
		go readStdinCommands()
	}

	presetLabel := "default"
	if fastMode {
		presetLabel = "fast"
	}
	fmt.Printf("spankmac: listening for slaps in %s mode with %s tuning... (ctrl+c to quit)\n", pack.name, presetLabel)
	if stdioMode {
		fmt.Println(`{"status":"ready"}`)
	}

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case err := <-sensorErr:
			return fmt.Errorf("sensor worker failed: %w", err)
		case newPackName := <-packUpdates:
			newPack := loadPack(newPackName)
			tracker = newSlapTracker(newPack, tuning.cooldown)
			activePackName = newPackName
			pack = newPack
		case <-ticker.C:
		}

		// Check if paused
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if time.Since(lastYell) <= time.Duration(cooldownMs)*time.Millisecond {
			continue
		}
		if ev.Amplitude < minAmplitude {
			continue
		}

		lastYell = now
		num, score := tracker.record(now)
		file := tracker.getFile(score)
		if stdioMode || guiMode {
			event := GUIEvent{
				Timestamp:  now.Format(time.RFC3339Nano),
				SlapNumber: num,
				Amplitude:  ev.Amplitude,
				Severity:   string(ev.Severity),
				File:       file,
			}
			if stdioMode {
				if data, err := json.Marshal(event); err == nil {
					fmt.Println(string(data))
				}
			}
			if guiMode {
				select {
				case guiEvents <- event:
				default:
				}
			}
		} else {
			fmt.Printf("slap #%d [%s amp=%.5fg] -> %s\n", num, ev.Severity, ev.Amplitude, file)
		}
		go playAudio(pack, file, ev.Amplitude, &speakerInit)
	}
}

var speakerMu sync.Mutex

// amplitudeToVolume maps a detected amplitude to a beep/effects.Volume
// level. Amplitude typically ranges from ~0.05 (light tap) to ~1.0+
// (hard slap). The mapping uses a logarithmic curve so that light taps
// are noticeably quieter and hard hits play near full volume.
//
// Returns a value in the range [-3.0, 0.0] for use with effects.Volume
// (base 2): -3.0 is ~1/8 volume, 0.0 is full volume.
func amplitudeToVolume(amplitude float64) float64 {
	const (
		minAmp   = 0.05  // softest detectable
		maxAmp   = 0.80  // treat anything above this as max
		minVol   = -3.0  // quietest playback (1/8 volume with base 2)
		maxVol   = 0.0   // full volume
	)

	// Clamp
	if amplitude <= minAmp {
		return minVol
	}
	if amplitude >= maxAmp {
		return maxVol
	}

	// Normalize to [0, 1]
	t := (amplitude - minAmp) / (maxAmp - minAmp)

	// Log curve for more natural volume scaling
	// log(1 + t*99) / log(100) maps [0,1] -> [0,1] with a log curve
	t = math.Log(1+t*99) / math.Log(100)

	return minVol + t*(maxVol-minVol)
}

func playAudio(pack *soundPack, path string, amplitude float64, speakerInit *bool) {
	var streamer beep.StreamSeekCloser
	var format beep.Format

	if pack.custom {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spankmac: open %s: %v\n", path, err)
			return
		}
		defer file.Close()
		streamer, format, err = mp3.Decode(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spankmac: decode %s: %v\n", path, err)
			return
		}
	} else {
		data, err := pack.fs.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spankmac: read %s: %v\n", path, err)
			return
		}
		streamer, format, err = mp3.Decode(io.NopCloser(bytes.NewReader(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "spankmac: decode %s: %v\n", path, err)
			return
		}
	}
	defer streamer.Close()

	speakerMu.Lock()
	if !*speakerInit {
		speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
		*speakerInit = true
	}
	speakerMu.Unlock()

	// Optionally scale volume based on slap amplitude
	var source beep.Streamer = streamer
	if volumeScaling {
		source = &effects.Volume{
			Streamer: streamer,
			Base:     2,
			Volume:   amplitudeToVolume(amplitude),
			Silent:   false,
		}
	}

	// Apply speed change via resampling trick:
	// Claiming the audio is at rate*speed and resampling back to rate
	// makes the speaker consume samples faster/slower.
	if speedRatio != 1.0 && speedRatio > 0 {
		fakeRate := beep.SampleRate(int(float64(format.SampleRate) * speedRatio))
		source = beep.Resample(4, fakeRate, format.SampleRate, source)
	}

	done := make(chan bool)
	speaker.Play(beep.Seq(source, beep.Callback(func() {
		done <- true
	})))
	<-done
}

// stdinCommand represents a command received via stdin
type stdinCommand struct {
	Cmd       string  `json:"cmd"`
	Amplitude float64 `json:"amplitude,omitempty"`
	Cooldown  int     `json:"cooldown,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
}

// readStdinCommands reads JSON commands from stdin for live control
func readStdinCommands() {
	processCommands(os.Stdin, os.Stdout)
}

// processCommands reads JSON commands from r and writes responses to w.
// This is the testable core of the stdin command handler.
func processCommands(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var cmd stdinCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			if stdioMode {
				fmt.Fprintf(w, `{"error":"invalid command: %s"}%s`, err.Error(), "\n")
			}
			continue
		}

		switch cmd.Cmd {
		case "pause":
			pausedMu.Lock()
			paused = true
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"paused"}`)
			}
		case "resume":
			pausedMu.Lock()
			paused = false
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"resumed"}`)
			}
		case "set":
			if cmd.Amplitude > 0 && cmd.Amplitude <= 1 {
				minAmplitude = cmd.Amplitude
			}
			if cmd.Cooldown > 0 {
				cooldownMs = cmd.Cooldown
			}
			if cmd.Speed > 0 {
				speedRatio = cmd.Speed
			}
			if stdioMode {
				fmt.Fprintf(w, `{"status":"settings_updated","amplitude":%.4f,"cooldown":%d,"speed":%.2f}%s`, minAmplitude, cooldownMs, speedRatio, "\n")
			}
		case "volume-scaling":
			volumeScaling = !volumeScaling
			if stdioMode {
				fmt.Fprintf(w, `{"status":"volume_scaling_toggled","volume_scaling":%t}%s`, volumeScaling, "\n")
			}
		case "status":
			pausedMu.RLock()
			isPaused := paused
			pausedMu.RUnlock()
			if stdioMode {
				fmt.Fprintf(w, `{"status":"ok","paused":%t,"amplitude":%.4f,"cooldown":%d,"volume_scaling":%t,"speed":%.2f}%s`, isPaused, minAmplitude, cooldownMs, volumeScaling, speedRatio, "\n")
			}
		default:
			if stdioMode {
				fmt.Fprintf(w, `{"error":"unknown command: %s"}%s`, cmd.Cmd, "\n")
			}
		}
	}
}

func startWebServer() {
	staticFS, err := fs.Sub(guiFS, "gui")
	if err != nil {
		panic(err)
	}
	http.Handle("/", http.FileServer(http.FS(staticFS)))
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch := make(chan GUIEvent, 50)
		guiClients.Store(ch, true)
		defer guiClients.Delete(ch)

		for {
			select {
			case ev := <-ch:
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	go func() {
		for ev := range guiEvents {
			guiClients.Range(func(key, value any) bool {
				ch := key.(chan GUIEvent)
				select {
				case ch <- ev:
				default:
				}
				return true
			})
		}
	}()
	http.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		if data, err := os.ReadFile("assets/logo.png"); err == nil {
			w.Header().Set("Content-Type", "image/png")
			w.Write(data)
			return
		}
		if data, err := assetsFS.ReadFile("assets/logo.png"); err == nil {
			w.Header().Set("Content-Type", "image/png")
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	})
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]any{
			"paused":         isPaused,
			"amplitude":      minAmplitude,
			"cooldown":       cooldownMs,
			"volume_scaling": volumeScaling,
			"speed":          speedRatio,
			"pack":           activePackName,
		})
	})
	http.HandleFunc("/api/pause", func(w http.ResponseWriter, r *http.Request) {
		pausedMu.Lock()
		paused = true
		pausedMu.Unlock()
	})
	http.HandleFunc("/api/resume", func(w http.ResponseWriter, r *http.Request) {
		pausedMu.Lock()
		paused = false
		pausedMu.Unlock()
	})
	http.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Amplitude *float64 `json:"amplitude"`
			Cooldown  *int     `json:"cooldown"`
			Speed     *float64 `json:"speed"`
			ToggleVol *bool    `json:"toggle_volume_scaling"`
			Pack      *string  `json:"pack"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Amplitude != nil {
			minAmplitude = *req.Amplitude
		}
		if req.Cooldown != nil {
			cooldownMs = *req.Cooldown
		}
		if req.Speed != nil {
			speedRatio = *req.Speed
		}
		if req.ToggleVol != nil {
			volumeScaling = !volumeScaling
		}
		if req.Pack != nil {
			packUpdates <- *req.Pack
		}
	})
	http.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	})
	http.ListenAndServe("127.0.0.1:8080", nil)
}

func installApplet() error {
	binPath, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	appDir := filepath.Join(home, "Applications", "SpankMac.app")
	macOSDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0755); err != nil {
		return err
	}
	scriptPath := filepath.Join(macOSDir, "SpankMac")

	// Write Swift wrapper
	swiftPath := filepath.Join(macOSDir, "wrapper.swift")
	swiftCode := `import Cocoa
import WebKit

class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate {
    var window: NSWindow!
    var webView: WKWebView!

    func applicationDidFinishLaunching(_ notification: Notification) {
        let rect = NSRect(x: 0, y: 0, width: 900, height: 750)
        let mask: NSWindow.StyleMask = [.titled, .closable, .miniaturizable, .resizable]
        window = NSWindow(contentRect: rect, styleMask: mask, backing: .buffered, defer: false)
        window.title = "SpankMac"
        window.center()

        let config = WKWebViewConfiguration()
        webView = WKWebView(frame: rect, configuration: config)
        webView.navigationDelegate = self
        window.contentView = webView
        
        loadApp()

        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func loadApp() {
        let url = URL(string: "http://127.0.0.1:8080")!
        webView.load(URLRequest(url: url))
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
            self.loadApp()
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        let task = Process()
        task.launchPath = "/usr/bin/curl"
        task.arguments = ["-X", "POST", "http://127.0.0.1:8080/api/quit"]
        try? task.run()
        task.waitUntilExit()
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
`
	if err := os.WriteFile(swiftPath, []byte(swiftCode), 0644); err != nil {
		return err
	}

	uiPath := filepath.Join(macOSDir, "SpankMacUI")
	cmdCompiler := exec.Command("swiftc", swiftPath, "-o", uiPath)
	if err := cmdCompiler.Run(); err != nil {
		return fmt.Errorf("failed to compile swift wrapper: %w", err)
	}
	os.Remove(swiftPath)

	// Generate App Icon if logo exists
	logoData, err := os.ReadFile("assets/logo.png")
	if err != nil {
		logoData, _ = assetsFS.ReadFile("assets/logo.png")
	}
	if len(logoData) > 0 {
		resourcesDir := filepath.Join(appDir, "Contents", "Resources")
		os.MkdirAll(resourcesDir, 0755)
		logoPath := filepath.Join(resourcesDir, "logo.png")
		os.WriteFile(logoPath, logoData, 0644)

		iconsetDir := filepath.Join(resourcesDir, "AppIcon.iconset")
		os.MkdirAll(iconsetDir, 0755)

		sizes := []int{16, 32, 64, 128, 256, 512}
		for _, size := range sizes {
			s := fmt.Sprintf("%d", size)
			exec.Command("sips", "-z", s, s, logoPath, "--out", filepath.Join(iconsetDir, fmt.Sprintf("icon_%dx%d.png", size, size))).Run()
			exec.Command("sips", "-z", fmt.Sprintf("%d", size*2), fmt.Sprintf("%d", size*2), logoPath, "--out", filepath.Join(iconsetDir, fmt.Sprintf("icon_%dx%d@2x.png", size, size))).Run()
		}
		exec.Command("iconutil", "-c", "icns", iconsetDir, "-o", filepath.Join(resourcesDir, "AppIcon.icns")).Run()
		os.RemoveAll(iconsetDir)
		os.Remove(logoPath)
	}

	// Write Info.plist
	infoPlistPath := filepath.Join(appDir, "Contents", "Info.plist")
	infoPlistContent := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>SpankMac</string>
    <key>CFBundleIconFile</key>
    <string>AppIcon</string>
    <key>CFBundleIdentifier</key>
    <string>com.taigrr.spankmac</string>
    <key>CFBundleName</key>
    <string>SpankMac</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
</dict>
</plist>`
	if err := os.WriteFile(infoPlistPath, []byte(infoPlistContent), 0644); err != nil {
		return err
	}

	scriptContent := fmt.Sprintf("#!/bin/bash\nDIR=\"$( cd \"$( dirname \"${BASH_SOURCE[0]}\" )\" && pwd )\"\n\"%s\" gui --native &\n\"$DIR/SpankMacUI\"\n", binPath)
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		return err
	}
	fmt.Printf("Installed SpankMac.app to %s\nDouble-click it in Finder to start the SpankMac GUI.\n", appDir)
	return nil
}
