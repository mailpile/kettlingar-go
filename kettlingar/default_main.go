package kettlingar

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	ExitOK = iota
	ExitCobraFailed
	ExitAlreadyRunning
	ExitSetupFailed
	ExitBackgroundFailed
	ExitStartupFailed
)

// These interfaces define process-lifecycle hooks, in the order they run.
type ServiceWithSetup interface {
	ServiceSetup(*KettlingarService) error
}

type ServiceWithBackground interface {
	ServiceBackground(*KettlingarService) error
}

type ServiceWithStartup interface {
	ServiceStartup(*KettlingarService) error
}

type ServiceWithShutdown interface {
	ServiceShutdown(*KettlingarService) error
}

var outFormat string = "text"
var defaultURL string = "http://localhost:8123"

func (ks *KettlingarService) DefaultMain(mainArg0 string, mainArgs []string) {
	// Initialize Viper
	viper.SetEnvPrefix(strings.ReplaceAll(strings.ToUpper(ks.Name), "-", "_"))
	viper.AutomaticEnv()
	// Env vars use UPPER_CASE_WITH_UNDERSCORES: dashed flag names and the
	// "command.flag" keys used for RPC arguments both map via these replacements.
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))

	configDir := filepath.Dir(ks.getStateFilePath())
	viper.AddConfigPath(configDir)
	viper.SetConfigName(ks.Name)
	viper.SetConfigType("yaml")
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			ks.Logger.Warn(ks.Name+": error reading config file", "err", err)
		}
	}

	rootCmd := &cobra.Command{
		Use:   ks.Name,
		Short: ks.Name + " " + ks.Version + " RPC CLI",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if ks.Url == "" {
				ks.Url = viper.GetString("url")
			}
		},
	}

	rootCmd.PersistentFlags().StringVarP(&ks.Url, "url", "u", "", "URL of server")
	viper.BindPFlag("url", rootCmd.PersistentFlags().Lookup("url"))

	rootCmd.PersistentFlags().StringVarP(&outFormat,
		"format", "f", "text", "text, json, msgpack, ...")

	// 1. Setup Static Commands
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start server",
		Run: func(cmd *cobra.Command, args []string) {
			ks.syncServiceConfigs()
			ks.startServer(cmd)
		},
	}
	startCmd.Flags().StringP("port", "p", "8080", "Port to listen on")
	startCmd.Flags().BoolP("foreground", "F", false, "Run in foreground")
	ks.addServiceFlags(startCmd) // Iterate over ks.services, add app-specific flags
	rootCmd.AddCommand(startCmd)

	rootCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop server (via PID in state file)",
		Run: func(cmd *cobra.Command, args []string) {
			ks.probeOrStopServer(false)
		},
	})

	// 2. Custom Help Logic
	originalHelp := rootCmd.HelpFunc()
	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		// Try to augment help with live data
		rootCmd.PersistentFlags().Parse(mainArgs)
		ks.autoDiscover()

		which := ks.StateFn
		if which == "" {
			which = ks.Url
		}
		be_status := fmt.Sprintf(
			"\nService is down, not reachable via %s\n\n", which)

		manifest, err := ks.fetchManifest()
		if err == nil {
			be_status = fmt.Sprintf(
				"\nService is up!\n - URL: %s\n - File: %s\n\n",
				ks.Url, ks.StateFn)
			// Augment commands if they haven't been added yet
			for _, m := range manifest {
				if findSubCommand(rootCmd, m.Name) == nil {
					rootCmd.AddCommand(ks.createRpcCommand(m))
				}
			}
		}
		originalHelp(cmd, args)
		fmt.Print(be_status)
	})

	// 3. Dynamic API Discovery for direct execution
	if len(mainArgs) > 0 && !strings.HasPrefix(mainArgs[0], "-") && mainArgs[0] != "start" && mainArgs[0] != "stop" && mainArgs[0] != "help" {
		rootCmd.PersistentFlags().Parse(mainArgs)
		ks.autoDiscover()
		manifest, err := ks.fetchManifest()
		if err == nil {
			for _, m := range manifest {
				if findSubCommand(rootCmd, m.Name) == nil {
					rootCmd.AddCommand(ks.createRpcCommand(m))
				}
			}
		}
	}

	rootCmd.SetArgs(mainArgs)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(ExitCobraFailed)
	}
}

func registerFlag(flags *pflag.FlagSet, name, def, help string, t reflect.Type) {
	// Check for specific named types first
	switch t {
	case reflect.TypeOf(time.Duration(0)):
		d, _ := time.ParseDuration(def)
		flags.Duration(name, d, help)
		return
	case reflect.TypeOf(time.Time{}):
		// We validate the default string is a valid date/time
		flags.String(name, def, fmt.Sprintf("%s (Format: RFC3339 or YYYY-MM-DD)", help))
		return
	}

	// Fall back to checking primitives
	switch t.Kind() {
	case reflect.Bool:
		d, _ := strconv.ParseBool(def)
		flags.Bool(name, d, help)

	case reflect.Int, reflect.Int64:
		d, _ := strconv.ParseInt(def, 10, 64)
		flags.Int64(name, d, help)

	case reflect.Float64, reflect.Float32:
		d, _ := strconv.ParseFloat(def, 64)
		flags.Float64(name, d, help)

	default:
		// netip.Addr and others fall back to String
		flags.String(name, def, help)
	}
}

// Add service flags based on annotations in the interface
func (ks *KettlingarService) addServiceFlags(cmd *cobra.Command) {
	for _, svc := range ks.services {
		v := reflect.ValueOf(svc)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}

		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			help := field.Tag.Get("help")
			def := field.Tag.Get("default")

			if help == "" || def == "" {
				continue
			}

			flagName := dashedName(field.Name)

			// Use our extracted helper
			registerFlag(cmd.Flags(), flagName, def, help, field.Type)

			// Bind to Viper
			viper.BindPFlag(flagName, cmd.Flags().Lookup(flagName))
		}
	}
}

// Update our service config based on flags/viper settings configurd above
func (ks *KettlingarService) syncServiceConfigs() {
	for _, svc := range ks.services {
		v := reflect.ValueOf(svc)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}
		t := v.Type()

		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Tag.Get("help") == "" || field.Tag.Get("default") == "" {
				continue
			}

			flagName := dashedName(field.Name)
			f := v.Field(i)

			if f.CanSet() {
				// Handle Named Types First
				if f.Type() == reflect.TypeOf(time.Duration(0)) {
					f.SetInt(int64(viper.GetDuration(flagName)))
					continue
				}

				if f.Type() == reflect.TypeOf(time.Time{}) {
					valStr := viper.GetString(flagName)
					// Try RFC3339 first, then fallback to Date only
					if tm, err := time.Parse(time.RFC3339, valStr); err == nil {
						f.Set(reflect.ValueOf(tm))
					} else if tm, err := time.Parse("2006-01-02", valStr); err == nil {
						f.Set(reflect.ValueOf(tm))
					} else {
						ks.Logger.Error("invalid date format", "field", flagName, "value", valStr)
					}
					continue
				}

				// Handle Primitive Kinds
				switch f.Kind() {
				case reflect.String:
					f.SetString(viper.GetString(flagName))
				case reflect.Int, reflect.Int64:
					f.SetInt(viper.GetInt64(flagName))
				case reflect.Bool:
					f.SetBool(viper.GetBool(flagName))
				case reflect.Float64, reflect.Float32:
					f.SetFloat(viper.GetFloat64(flagName))
				case reflect.Struct:
					// netip.Addr is handled here as it is a struct kind
					if f.Type() == reflect.TypeOf(netip.Addr{}) {
						valStr := viper.GetString(flagName)
						if addr, err := netip.ParseAddr(valStr); err == nil {
							f.Set(reflect.ValueOf(addr))
						} else {
							ks.Logger.Error("invalid IP address", "field", flagName, "value", valStr)
						}
					}
				}
			}
		}
	}
}

func (ks *KettlingarService) runSetupFunctions() error {
	for _, svc := range ks.services {
		if v, ok := svc.(ServiceWithSetup); ok {
			if err := v.ServiceSetup(ks); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ks *KettlingarService) runStartupFunctions() error {
	for _, svc := range ks.services {
		if v, ok := svc.(ServiceWithStartup); ok {
			if err := v.ServiceStartup(ks); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ks *KettlingarService) runShutdownFunctions() {
	for _, svc := range ks.services {
		if v, ok := svc.(ServiceWithShutdown); ok {
			if err := v.ServiceShutdown(ks); err != nil {
				ks.Logger.Error(ks.Name+": service shutdown error", "err", err)
			}
		}
	}
}

func (ks *KettlingarService) runBackgroundFunction() bool {
	for _, svc := range ks.services {
		if v, ok := svc.(ServiceWithBackground); ok {
			if err := v.ServiceBackground(ks); err != nil {
				ks.Logger.Error(ks.Name+": daemonizing failed", "err", err)
				os.Exit(ExitBackgroundFailed)
			}
			return true
		}
	}
	return false
}

// dashedName converts a CamelCase argument name to a lower-case dashed flag
// name, e.g. "MaxConns" -> "max-conns" and "HTTPProxy" -> "http-proxy". A dash
// is inserted before an upper-case rune that starts a new word: one preceded by
// a lower-case letter or digit, or one ending an acronym before a lower-case
// letter.
func dashedName(name string) string {
	runes := []rune(name)
	var b strings.Builder
	addedDash := false
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			var next rune
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if unicode.IsLower(prev) || unicode.IsDigit(prev) ||
				(unicode.IsUpper(prev) && unicode.IsLower(next)) {
				if !addedDash {
					b.WriteRune('-')
					addedDash = true
				}
			} else {
				addedDash = false
			}
		} else {
			addedDash = false
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// isPositionalArgs reports whether a method argument is the special "Args"
// field that collects positional CLI arguments rather than a --flag. It is an
// []string, which the manifest records with an empty type name.
func isPositionalArgs(name, typeName string) bool {
	return name == "Args" && typeName == ""
}

func methodHasPositionalArgs(m MethodDesc) bool {
	for aName, aType := range m.Args {
		if isPositionalArgs(aName, aType) {
			return true
		}
	}
	return false
}

// Helper to create a Cobra command from a MethodDesc
func (ks *KettlingarService) createRpcCommand(m MethodDesc) *cobra.Command {
	use := m.Name
	if methodHasPositionalArgs(m) {
		use += " [args...]"
	}
	long := m.Docs
	if extra := cliRenderFormats(m.ReturnType); extra != "" {
		long = strings.TrimRight(long, "\n") + fmt.Sprintf(
			"\n\nExtra -f formats for this command: %s", extra)
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: m.Help, // Used in the command list
		Long:  long,   // Shown when specifically calling 'help <cmd>'
		Run: func(cmd *cobra.Command, args []string) {
			ks.doCall(m, cmd, args)
		},
	}
	for aName, aType := range m.Args {
		if isPositionalArgs(aName, aType) {
			continue // collected from positional args, not a flag
		}
		flagName := dashedName(aName)
		defaultArgs := ""
		if def, ok := m.ArgDefaults[aName]; ok {
			defaultArgs = def
		}
		cmd.Flags().String(flagName, defaultArgs, fmt.Sprintf("(%s)", aType))
		viperKey := fmt.Sprintf("%s.%s", m.Name, flagName)
		viper.BindPFlag(viperKey, cmd.Flags().Lookup(flagName))
	}
	return cmd
}

// cliRenderFormats asks a method's return type which extra -f format names it
// supports, by calling Render("?") on a zero value. The convention is that
// Render("?") returns a comma-separated list of short CLI names (no MIME slash);
// a type that does not implement the convention returns its default MIME (which
// contains a slash) and is treated as having no extras. Returns "" when there
// are none.
func cliRenderFormats(rt reflect.Type) string {
	if rt == nil {
		return ""
	}
	v, ok := reflect.New(rt).Interface().(DataRenderer)
	if !ok {
		return ""
	}
	list, _ := v.Render("?")
	if list == "" || strings.Contains(list, "/") {
		return ""
	}
	return list
}

// Helper to check if a command already exists
func findSubCommand(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func (ks *KettlingarService) startServer(cmd *cobra.Command) {
	port, _ := cmd.Flags().GetString("port")
	foreground, _ := cmd.Flags().GetBool("foreground")
	baseURL := fmt.Sprintf("http://localhost:%s", port)
	statePath := ks.getStateFilePath()

	ks.autoDiscover()
	if ks.Url != defaultURL {
		if ks.probeOrStopServer(true) {
			ks.Logger.Error(ks.Name+": already running", "url", ks.Url, "file", ks.StateFn)
			os.Exit(ExitAlreadyRunning)
		}
	}

	if err := ks.runSetupFunctions(); err != nil {
		ks.Logger.Error(ks.Name+": setup failed", "err", err)
		os.Exit(ExitSetupFailed)
	}

	if !foreground {
		if !ks.runBackgroundFunction() {
			if err := ks.spawnBackground(); err != nil {
				ks.Logger.Error(ks.Name+": spawn background failed", "err", err)
				os.Exit(ExitStartupFailed)
			}
		}
		os.Exit(ExitOK)
	}

	ks.Url = fmt.Sprintf("%s/%s", baseURL, ks.Secret)
	stateData := fmt.Sprintf("%s\n%d", ks.Url, os.Getpid())
	os.WriteFile(statePath, []byte(stateData), 0600)

	srv := &http.Server{Handler: ks.Mux}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	if err := ks.runStartupFunctions(); err != nil {
		signalReady("ERR startup failed: " + err.Error())
		ks.Logger.Error(ks.Name+": startup failed", "err", err)
		os.Exit(ExitStartupFailed)
	}

	// Bind the listener before signaling readiness: once the socket is bound
	// the kernel queues connections, so a parent told "ready" (and any CLI call
	// after it) will not see connection-refused. Startup hooks have already run,
	// so the service is fully configured by the time we accept.
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		signalReady("ERR listen failed: " + err.Error())
		ks.Logger.Error(ks.Name+": listen failed", "port", port, "err", err)
		os.Remove(statePath)
		os.Exit(ExitStartupFailed)
	}
	signalReady("OK")

	go func() {
		fmt.Printf("%s listening on %s/%s (PID: %d)\n", ks.Name, baseURL, ks.Secret, os.Getpid())
		ks.Logger.Info(ks.Name+": listening", "url", baseURL, "pid", os.Getpid())
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			ks.Logger.Error(ks.Name+": http server error", "err", err)
		}
	}()

	<-stop
	fmt.Println("\nShutting down...")
	ks.Logger.Info(ks.Name + ": shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	ks.runShutdownFunctions()
	os.Remove(statePath)
}

// readyFdEnv names the inherited pipe file descriptor a background child uses to
// report readiness to the parent that spawned it. startupTimeout bounds how long
// the parent waits for that signal before giving up.
const (
	readyFdEnv     = "KETTLINGAR_READY_FD"
	startupTimeout = 30 * time.Second
)

// spawnBackground re-execs the process in --foreground mode and waits for it to
// report that it is fully started (configuration loaded and listener bound)
// before returning. This makes "start" a synchronous readiness barrier: when it
// returns success the service is actually able to serve requests, so a CLI
// command issued immediately afterwards cannot race an unready server.
func (ks *KettlingarService) spawnBackground() error {
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("Error daemonizing: %v", err)
	}
	defer readPipe.Close()

	args := append(os.Args[1:], "--foreground")
	newCmd := exec.Command(os.Args[0], args...)
	newCmd.ExtraFiles = []*os.File{writePipe} // becomes fd 3 in the child
	newCmd.Env = append(os.Environ(), fmt.Sprintf("%s=3", readyFdEnv))
	if err := newCmd.Start(); err != nil {
		writePipe.Close()
		return fmt.Errorf("Error daemonizing: %v", err)
	}
	// The child holds the write end now; drop ours so we observe EOF if it dies
	// without signaling.
	writePipe.Close()

	if err := ks.awaitChildReady(readPipe, newCmd); err != nil {
		return fmt.Errorf("Failed to start %s: %v", ks.Name, err)
	}
	fmt.Printf("%s started in background (PID: %d)\n", ks.Name, newCmd.Process.Pid)
	ks.Logger.Info(ks.Name+": started in background", "pid", newCmd.Process.Pid)
	return nil
}

// awaitChildReady blocks until the background child signals readiness over the
// pipe, reports a startup error, dies, or the timeout elapses.
func (ks *KettlingarService) awaitChildReady(r *os.File, cmd *exec.Cmd) error {
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r) // returns at EOF, i.e. when the child closes its end
		done <- strings.TrimSpace(string(data))
	}()
	select {
	case line := <-done:
		switch {
		case strings.HasPrefix(line, "OK"):
			return nil
		case strings.HasPrefix(line, "ERR "):
			cmd.Wait()
			return errors.New(strings.TrimPrefix(line, "ERR "))
		default:
			// Pipe closed with no status: the child exited before signaling.
			cmd.Wait()
			return errors.New("service exited before it became ready")
		}
	case <-time.After(startupTimeout):
		return fmt.Errorf("timed out after %s waiting for service to become ready", startupTimeout)
	}
}

// signalReady reports the child's startup outcome to the parent over the
// inherited pipe ("OK", or "ERR <message>"), then closes it so the parent's read
// unblocks. It is a no-op when not spawned with a readiness pipe (e.g. a manual
// "start --foreground"), so direct foreground runs are unaffected.
func signalReady(status string) {
	fdStr := os.Getenv(readyFdEnv)
	if fdStr == "" {
		return
	}
	os.Unsetenv(readyFdEnv) // signal at most once
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "kettlingar-ready")
	if f == nil {
		return
	}
	io.WriteString(f, status+"\n")
	f.Close()
}

func (ks *KettlingarService) Stop() bool {
	return ks.probeOrStopServer(false)
}

func (ks *KettlingarService) probeOrStopServer(onlyProbe bool) bool {
	statePath := ks.getStateFilePath()
	data, err := os.ReadFile(statePath)
	if err != nil {
		if !onlyProbe {
			ks.Logger.Warn(ks.Name+": server not running", "err", err)
		}
		return false
	}

	var pid int
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		os.Remove(statePath)
		return false
	}
	fmt.Sscanf(lines[1], "%d", &pid)

	sending := syscall.SIGTERM
	if onlyProbe {
		sending = 0
	}
	if process, err := os.FindProcess(pid); err == nil {
		if !onlyProbe {
			fmt.Printf("Stopping %s (PID: %d)...\n", ks.Name, pid)
			ks.Logger.Info(ks.Name+": stopping", "pid", pid)
		}
		if err := process.Signal(sending); err != nil {
			ks.Logger.Warn(ks.Name+": removing stale state", "file", statePath, "err", err)
			os.Remove(statePath)
			return false
		}
	}
	return true
}

func (ks *KettlingarService) getStateFilePath() string {
	cd, _ := os.UserConfigDir()
	path := filepath.Join(cd, "kettlingar")
	os.MkdirAll(path, 0700)
	return filepath.Join(path, ks.Name+".url")
}

func (ks *KettlingarService) autoDiscover() {
	if ks.Url == "" {
		ks.Url = viper.GetString("url")
	}
	if ks.Url == "" {
		ks.StateFn = ks.getStateFilePath()
		data, err := os.ReadFile(ks.StateFn)
		if err == nil {
			lines := strings.Split(string(data), "\n")
			ks.Url = strings.TrimSpace(lines[0])
		} else {
			ks.Url = defaultURL
		}
	}
}

func (ks *KettlingarService) fetchManifest() ([]MethodDesc, error) {
	// FIXME: Explicitly request msgpack, skip the json below
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(ks.Url + "/ping")
	if err != nil {
		// FIXME: Use this shortcut if the remote version is the
		//        same as ours?  Make that a thing we can detect.
		return ks.registry, err
	}

	defer resp.Body.Close()
	var pr PingResponse
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(resp.Header.Get("Content-Type"), "msgpack") {
		msgpack.Unmarshal(body, &pr)
	} else {
		json.Unmarshal(body, &pr)
	}

	// The CLI builds its commands from the locally compiled registry, so a
	// running service with a different API version may not accept them. This is a
	// warning state, not a hard failure: log it loudly but keep going.
	if pr.Version != "" && pr.Version != ks.Version {
		ks.Logger.Error("CLI/service API version mismatch",
			"cli_version", ks.Version, "service_version", pr.Version)
	}

	return ks.registry, nil
}

func (ks *KettlingarService) doCall(m MethodDesc, cmd *cobra.Command, posArgs []string) {
	params := make(map[string]interface{})
	for aName, aType := range m.Args {

		if isPositionalArgs(aName, aType) {
			if len(posArgs) > 0 {
				params[aName] = posArgs
			}
			continue
		}

		flagName := dashedName(aName)
		viperKey := fmt.Sprintf("%s.%s", m.Name, flagName)

		// Use viper.GetString to catch Env Vars or Config file entries
		v := viper.GetString(viperKey)

		if v != "" {
			val, err := parseValue(v, aType)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: flag --%s: %v\n", flagName, err)
				return
			}
			params[aName] = val
		}
	}

	payload, _ := msgpack.Marshal(params)
	req, _ := http.NewRequest("POST", ks.Url+"/"+m.Name, bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/msgpack")
	if outFormat == "msgpack" {
		req.Header.Set("Accept", "application/msgpack")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	// Trace the call without the URL: ks.Url embeds the auth secret.
	ks.Logger.Log(req.Context(), levelTrace, ks.Name+": cli rpc request",
		"method", m.Name, "generator", m.IsGenerator, "req_bytes", len(payload))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ks.Logger.Log(req.Context(), levelTrace, ks.Name+": cli rpc request failed",
			"method", m.Name, "err", err)
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()
	ks.Logger.Log(req.Context(), levelTrace, ks.Name+": cli rpc response",
		"method", m.Name, "status", resp.StatusCode)

	if m.IsGenerator {
		// Stream framed objects one at a time. A single Read can coalesce
		// several yielded values, so the body must be decoded by its framing
		// (msgpack self-delimits; JSON and SSE are line/blank-line delimited)
		// rather than treating each Read as one object.
		mimeType := resp.Header.Get("Content-Type")
		switch {
		case strings.Contains(mimeType, "msgpack"):
			dec := msgpack.NewDecoder(resp.Body)
			for {
				var raw msgpack.RawMessage
				if err := dec.Decode(&raw); err != nil {
					if err != io.EOF {
						fmt.Printf("Read error: %v\n", err)
					}
					break
				}
				printOutput(m, raw, mimeType)
			}
		case strings.Contains(mimeType, "event-stream"):
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
			for sc.Scan() {
				if line := sc.Text(); strings.HasPrefix(line, "data: ") {
					printOutput(m, []byte(strings.TrimPrefix(line, "data: ")), "application/json")
				}
			}
		default: // newline-delimited JSON (also used for text output)
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
			for sc.Scan() {
				if b := sc.Bytes(); len(strings.TrimSpace(string(b))) > 0 {
					printOutput(m, b, mimeType)
				}
			}
		}
	} else {
		if body, err := io.ReadAll(resp.Body); err == nil {
			printOutput(m, body, resp.Header.Get("Content-Type"))
		} else {
			fmt.Fprintf(os.Stderr, "Error reading")
		}
	}
}

func printOutput(m MethodDesc, data []byte, contentType string) {
	if strings.Contains(contentType, outFormat) {
		text := string(data)
		if outFormat == "msgpack" {
			fmt.Print(text)
		} else {
			fmt.Println(strings.TrimRight(text, "\r\n"))
		}
	} else if m.ReturnType != nil {
		valPtr := reflect.New(m.ReturnType)
		target := valPtr.Interface()
		if strings.Contains(contentType, "msgpack") {
			msgpack.Unmarshal(data, target)
		} else {
			json.Unmarshal(data, target)
		}
		if v, ok := target.(ProgressReporter); ok {
			progress := v.GetProgress()
			if progress.Progress != "" {
				fmt.Fprintf(os.Stderr, "%v\n", progress)
				if !progress.IsBoth {
					return
				}
			}
		}
		// For a non-default format the server sent as JSON/msgpack, ask the
		// return type to render it client-side via its Render method (the same
		// way "text" is produced by String). This makes formats like yaml that
		// kettlingar does not negotiate natively reachable through -f.
		if outFormat != "text" {
			if r, ok := target.(DataRenderer); ok {
				if _, rendered := r.Render(outFormat); rendered != nil {
					os.Stdout.Write(rendered)
					return
				}
			}
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("%v", target), "\r\n"))
	} else {
		var v interface{}
		if strings.Contains(contentType, "msgpack") {
			msgpack.Unmarshal(data, &v)
		} else {
			json.Unmarshal(data, &v)
		}
		fmt.Printf("%v\n", v)
	}
}

func parseValue(s string, typeName string) (interface{}, error) {
	switch typeName {
	case "string":
		return s, nil

	case "netip.Addr":
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid IP address '%s': %w", s, err)
		}
		return addr, nil

	case "int", "int64":
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer '%s' for type %s", s, typeName)
		}
		if typeName == "int" {
			return int(i), nil
		}
		return i, nil

	case "bool":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("invalid boolean '%s'", s)
		}
		return b, nil

	case "float64":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float '%s'", s)
		}
		return f, nil

	default:
		// Complex types (structs, maps, slices)
		// We attempt to parse as JSON.
		var val interface{}
		if err := json.Unmarshal([]byte(s), &val); err != nil {
			return nil, fmt.Errorf("could not parse complex type %s: %w", typeName, err)
		}
		return val, nil
	}
}
