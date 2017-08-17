package muniverse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unixpickle/essentials"
	"github.com/unixpickle/muniverse/chrome"
)

const (
	portRange    = "9000-9999"
	defaultImage = "unixpickle/muniverse:0.107.0"
)

const (
	callTimeout           = time.Minute * 2
	chromeConnectAttempts = 20
)

// This error message occurs very infrequently when doing
// `docker run` on my machine running Ubuntu 16.04.1.
const occasionalDockerErr = "Error response from daemon: device or resource busy."

// An Env controls and observes an environment.
//
// It is not safe to run an methods on an Env from more
// than one Goroutine at a time.
//
// The lifecycle of an environment is as follows:
// First, Reset is called to start an episode.
// Then, Step and Observe may be called repeatedly in any
// order until Step returns done=true to signal that the
// episode has ended.
// Once the episode has ended, Observe may be called but
// Step may not be.
// Call Reset to start a new episode and begin the process
// over again.
//
// When you are done with an Env, you must close it to
// clean up resources associated with it.
type Env interface {
	// Spec returns details about the environment.
	Spec() *EnvSpec

	// Reset resets the environment to a start state.
	Reset() error

	// Step sends the given events and advances the
	// episode by the given amount of time.
	//
	// If done is true, then the episode has ended.
	// After an episode ends, Reset must be called once
	// before Step may be called again.
	// However, observations may be made even after the
	// episode has ended.
	//
	// Typical event types are *chrome.MouseEvent and
	// *chrome.KeyEvent.
	Step(t time.Duration, events ...interface{}) (reward float64,
		done bool, err error)

	// Observe produces an observation for the current
	// state of the environment.
	Observe() (Obs, error)

	// Close cleans up resources used by the environment.
	//
	// After Close is called, the Env should not be used
	// anymore by any Goroutine.
	Close() error

	// Log returns internal log messages.
	// For example, it might return information about 404
	// errors.
	//
	// The returned list is a copy and may be modified by
	// the caller.
	Log() []string
}

type rawEnv struct {
	spec     EnvSpec
	gameHost string

	containerID string
	devConn     *chrome.Conn
	lastScore   float64

	needsReset         bool
	hasNavigatedBefore bool

	compression        bool
	compressionQuality int

	// Used to garbage collect the container if we
	// exit ungracefully.
	killSocket net.Conn
}

// Options specifies how to configure a new Env.
//
// By default, environments run inside of Docker.
// In this case, the Docker image can be manually
// specified with CustomImage, and/or a custom games
// directory can be specified with GamesDir.
//
// Sometimes, it might be desirable to run an Env directly
// inside an arbitrary instance of Chrome without starting
// a Docker container.
// In this case, you can set the DevtoolsHost field.
// You will also need to manually start a file-system
// server for your downloaded_games directory and point
// GameHost to this server.
type Options struct {
	// CustomImage, if non-empty, specifies a custom
	// Docker image to use for the environment.
	CustomImage string

	// GamesDir, if non-empty, specifies a directory on
	// the host to mount into the Docker container as a
	// downloaded_games folder.
	GamesDir string

	// DevtoolsHost, if non-empty, specifies the host of
	// an already-running Chrome's DevTools server.
	//
	// If this is non-empty, a Docker container will not
	// be launched, and the CustomImage and GamesDir
	// options should not be used.
	// Further, the GameHost field must be set.
	DevtoolsHost string

	// GameHost, if non-empty, specifies the host for the
	// downloaded_games file server.
	// This can only be used in conjunction with
	// DevtoolsHost.
	GameHost string

	// Compression, if set, allows observations to be
	// compressed in a lossy fashion.
	// Using compression may provide a performance boost, but
	// at the expense of observation integrity.
	Compression bool

	// CompressionQuality controls the quality of compressed
	// observations if Compression is set.
	// The value ranges from 0 to 100 (inclusive).
	CompressionQuality int
}

// NewEnv creates a new environment inside the default
// Docker image with the default settings.
//
// This may take a few minutes to run the first time,
// since it has to download a large Docker image.
// If the download takes too long, NewEnv may time out.
// If this happens, it is recommended that you download
// the image manually.
func NewEnv(spec *EnvSpec) (Env, error) {
	return NewEnvOptions(spec, &Options{})
}

// NewEnvOptions creates a new environment with the given
// set of options.
func NewEnvOptions(spec *EnvSpec, opts *Options) (e Env, err error) {
	defer essentials.AddCtxTo("create environment", &err)
	var res *rawEnv
	if opts.DevtoolsHost != "" {
		if opts.GameHost == "" {
			return nil, errors.New("must set GameHost with DevtoolsHost")
		}
		res, err = newEnvChrome(opts.DevtoolsHost, opts.GameHost, spec)
		if err != nil {
			return nil, err
		}
	} else {
		image := opts.CustomImage
		if image == "" {
			image = defaultImage
		}
		res, err = newEnvDocker(image, opts.GamesDir, spec)
		if err != nil {
			return nil, err
		}
	}
	res.compression = opts.Compression
	res.compressionQuality = opts.CompressionQuality
	return res, nil
}

func newEnvDocker(image, volume string, spec *EnvSpec) (env *rawEnv, err error) {
	ctx, cancel := callCtx()
	defer cancel()

	var id string

	fmt.Println("Trying docker run...")

	// Retry as a workaround for an occasional error given
	// by `docker run`.
	for i := 0; i < 3; i++ {
		id, err = dockerRun(ctx, image, volume, spec)
		if err != nil {
			fmt.Println("run failed", err)
		}
		if err == nil || !strings.Contains(err.Error(), occasionalDockerErr) {
			break
		}
	}

	if err != nil {
		return
	}

	fmt.Println("Getting ports and address...")

	ports, err := dockerBoundPorts(ctx, id)
	if err != nil {
		return
	}

	addr, err := dockerIPAddress(ctx, id)
	if err != nil {
		return
	}

	fmt.Println("address is:", addr)
	fmt.Println("ports are:", ports)

	conn, err := connectDevTools(ctx, addr+":"+ports["9222/tcp"])
	if err != nil {
		fmt.Println("failed to connect to devtools:", err)
		return
	}

	fmt.Println("connected to devtools")

	killSock, err := (&net.Dialer{}).DialContext(ctx, "tcp",
		addr+":"+ports["1337/tcp"])
	if err != nil {
		fmt.Println("failed to connect to kill socket:", err)
		conn.Close()
		return
	}

	fmt.Println("created environment!")

	return &rawEnv{
		spec:        *spec,
		gameHost:    "localhost",
		containerID: id,
		devConn:     conn,
		killSocket:  killSock,
	}, nil
}

func newEnvChrome(host, gameHost string, spec *EnvSpec) (*rawEnv, error) {
	ctx, cancel := callCtx()
	defer cancel()

	conn, err := connectDevTools(ctx, host)
	if err != nil {
		return nil, err
	}

	return &rawEnv{
		spec:       *spec,
		gameHost:   gameHost,
		devConn:    conn,
		needsReset: true,
	}, nil
}

func (r *rawEnv) Spec() *EnvSpec {
	res := r.spec
	return &res
}

func (r *rawEnv) Reset() (err error) {
	defer essentials.AddCtxTo("reset environment", &err)

	ctx, cancel := callCtx()
	defer cancel()

	if !r.hasNavigatedBefore {
		err = r.devConn.NavigateSafe(ctx, r.envURL())
		if err != nil {
			return
		}
		r.hasNavigatedBefore = true
	} else {
		err = r.devConn.NavigateSync(ctx, r.envURL())
		if err != nil {
			return
		}
	}

	var is404 bool
	check404 := "Promise.resolve(!window.muniverse && document.title.startsWith('404'));"
	err = r.devConn.EvalPromise(ctx, check404, &is404)
	if err != nil {
		return
	}
	if is404 {
		return errors.New("likely 404 page (no base game found)")
	}

	initCode := "window.muniverse.init(" + r.spec.Options + ");"
	err = r.devConn.EvalPromise(ctx, initCode, nil)
	if err != nil {
		return
	}

	err = r.devConn.EvalPromise(ctx, "window.muniverse.score();", &r.lastScore)
	err = essentials.AddCtx("get score", err)

	if err == nil {
		r.needsReset = false
	}

	return
}

func (r *rawEnv) Step(t time.Duration, events ...interface{}) (reward float64,
	done bool, err error) {
	defer essentials.AddCtxTo("step environment", &err)

	if r.needsReset {
		err = errors.New("environment needs reset")
		return
	}

	ctx, cancel := callCtx()
	defer cancel()

	for _, event := range events {
		switch event := event.(type) {
		case *chrome.MouseEvent:
			err = r.devConn.DispatchMouseEvent(ctx, event)
		case *chrome.KeyEvent:
			if r.allowKeyCode(event.Code) {
				err = r.devConn.DispatchKeyEvent(ctx, event)
			}
		default:
			err = fmt.Errorf("unsupported event type: %T", event)
		}
		if err != nil {
			return
		}
	}

	millis := int(t / time.Millisecond)
	timeStr := strconv.Itoa(millis)
	err = r.devConn.EvalPromise(ctx, "window.muniverse.step("+timeStr+");", &done)
	if err != nil {
		return
	}

	if done {
		r.needsReset = true
	}

	lastScore := r.lastScore
	err = r.devConn.EvalPromise(ctx, "window.muniverse.score();", &r.lastScore)
	if err != nil {
		err = essentials.AddCtx("get score", err)
		return
	}
	reward = r.lastScore - lastScore

	return
}

func (r *rawEnv) Observe() (obs Obs, err error) {
	defer essentials.AddCtxTo("observe environment", &err)

	ctx, cancel := callCtx()
	defer cancel()

	if !r.compression {
		if r.spec.AllCanvas {
			return r.observeCanvas(ctx)
		} else {
			data, err := r.devConn.ScreenshotPNG(ctx)
			if err != nil {
				return nil, err
			}
			return pngObs(data), nil
		}
	} else {
		data, err := r.devConn.ScreenshotJPEG(ctx, r.compressionQuality)
		if err != nil {
			return nil, err
		}
		return jpegObs(data), nil
	}
}

func (r *rawEnv) observeCanvas(ctx context.Context) (Obs, error) {
	var pngData []byte
	code := fmt.Sprintf(`
		Promise.resolve((function() {
			var canvas = document.getElementsByTagName('canvas')[0];

			// Most of the time, canvases are scaled for retina
			// displays or for the largest supported window size.
			var desiredWidth = %d;
			var desiredHeight = %d;
			if (canvas.width !== desiredWidth || canvas.height !== desiredHeight) {
				var dst = document.createElement('canvas');
				dst.width = desiredWidth;
				dst.height = desiredHeight;
				dst.getContext('2d').drawImage(canvas, 0, 0, desiredWidth, desiredHeight);
				canvas = dst;
			}

			var prefixLen = 'data:image/png;base64,'.length;
			return canvas.toDataURL('image/png').slice(prefixLen);
		})());
	`, r.spec.Width, r.spec.Height)
	if err := r.devConn.EvalPromise(ctx, code, &pngData); err != nil {
		return nil, err
	}
	return pngObs(pngData), nil
}

func (r *rawEnv) Close() (err error) {
	defer essentials.AddCtxTo("close environment", &err)

	ctx, cancel := callCtx()
	defer cancel()

	errs := []error{
		r.devConn.Close(),
	}
	if r.containerID != "" {
		_, e := dockerCommand(ctx, "kill", r.containerID)
		errs = append(errs, e)
	}

	if r.killSocket != nil {
		// TODO: look into if this can ever produce an error,
		// since the container might already have closed the
		// socket by now.
		//
		// We don't close this *before* stopping the container
		// since `docker kill` might fail if the container
		// already died and was cleaned up.
		r.killSocket.Close()
	}

	// Any calls after Close() should trigger simple errors.
	r.devConn = nil
	r.killSocket = nil

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *rawEnv) Log() []string {
	return r.devConn.ConsoleLog()
}

func (r *rawEnv) envURL() string {
	baseName := r.spec.Name
	if r.spec.VariantOf != "" {
		baseName = r.spec.VariantOf
	}
	return "http://" + r.gameHost + "/" + baseName
}

func (r *rawEnv) allowKeyCode(code string) bool {
	for _, c := range r.spec.KeyWhitelist {
		if c == code {
			return true
		}
	}
	return false
}

func callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

func dockerRun(ctx context.Context, container, volume string,
	spec *EnvSpec) (id string, err error) {
	args := []string{
		"run",
		"-p",
		portRange + ":9222",
		"-p",
		portRange + ":1337",
		"--shm-size=200m",
		"-d",   // Run in detached mode.
		"--rm", // Automatically delete the container.
		"-i",   // Give netcat a stdin to read from.
	}
	if volume != "" {
		if strings.Contains(volume, ":") {
			return "", errors.New("path contains colons: " + volume)
		}
		args = append(args, "-v", volume+":/downloaded_games")
	}
	args = append(args, container,
		fmt.Sprintf("--window-size=%d,%d", spec.Width, spec.Height))

	output, err := dockerCommand(ctx, args...)
	if err != nil {
		return "", essentials.AddCtx("docker run",
			fmt.Errorf("%s (make sure docker is up-to-date)", err))
	}

	return strings.TrimSpace(string(output)), nil
}

func dockerBoundPorts(ctx context.Context,
	containerID string) (mapping map[string]string, err error) {
	defer essentials.AddCtxTo("docker inspect", &err)
	rawJSON, err := dockerCommand(ctx, "inspect", containerID)
	if err != nil {
		return nil, err
	}
	var info []struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string
			}
		}
	}
	if err := json.Unmarshal(rawJSON, &info); err != nil {
		return nil, err
	}
	if len(info) != 1 {
		return nil, errors.New("unexpected number of results")
	}
	rawMapping := info[0].NetworkSettings.Ports
	mapping = map[string]string{}
	for containerPort, hostPorts := range rawMapping {
		if len(hostPorts) != 1 {
			return nil, errors.New("unexpected number of host ports")
		}
		mapping[containerPort] = hostPorts[0].HostPort
	}
	return
}

func dockerIPAddress(ctx context.Context, containerID string) (addr string, err error) {
	if runtime.GOOS != "windows" {
		return "localhost", nil
	}
	defer essentials.AddCtxTo("docker inspect", &err)
	for _, network := range []string{"bridge", "nat"} {
		ipData, err := dockerCommand(
			ctx,
			"inspect",
			"--format",
			"{{ .NetworkSettings.Networks."+network+".IPAddress }}",
			containerID,
		)
		if err != nil {
			return "", err
		}
		ipStr := strings.TrimSpace(string(ipData))
		if ipStr == "<no value>" || ipStr == "" {
			continue
		}
		return ipStr, nil
	}
	return "", errors.New("unable to find container IP address")
}

var dockerLock sync.Mutex

func dockerCommand(ctx context.Context, args ...string) (output []byte, err error) {
	dockerLock.Lock()
	defer dockerLock.Unlock()
	output, err = exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		if eo, ok := err.(*exec.ExitError); ok && len(eo.Stderr) > 0 {
			stderrMsg := strings.TrimSpace(string(eo.Stderr))
			err = fmt.Errorf("%s: %s", eo.String(), stderrMsg)
		}
	}
	return
}

func connectDevTools(ctx context.Context, host string) (conn *chrome.Conn,
	err error) {
	for i := 0; i < chromeConnectAttempts; i++ {
		conn, err = attemptDevTools(ctx, host)
		if err == nil {
			return
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return
}

func attemptDevTools(ctx context.Context, host string) (conn *chrome.Conn,
	err error) {
	endpoints, err := chrome.Endpoints(ctx, host)
	if err != nil {
		return
	}

	for _, ep := range endpoints {
		if ep.Type == "page" && ep.WebSocketURL != "" {
			fmt.Println("attempting websocket connection", ep.WebSocketURL)
			return chrome.NewConn(ctx, ep.WebSocketURL)
		}
	}

	return nil, errors.New("no Chrome page endpoint")
}
