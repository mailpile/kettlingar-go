package kettlingar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	ExitOK = iota
	ExitCobraFailed
	ExitBackgroundFailed
	ExitAlreadyRunning
	ExitStartupFailed
)

type ServiceWithStartup interface {
	ServiceStartup(*KettlingarService) error
}

type ServiceWithShutdown interface {
	ServiceShutdown(*KettlingarService) error
}

type ServiceWithBackground interface {
	ServiceBackground(*KettlingarService) error
}

var outFormat string = "text"
var defaultURL string = "http://localhost:8123"

func (ks *KettlingarService) DefaultMain(mainArg0 string, mainArgs []string) {
	// Initialize Viper
	viper.SetEnvPrefix(strings.ReplaceAll(strings.ToUpper(ks.Name), "-", "_"))
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	configDir := filepath.Dir(ks.getStateFilePath())
	viper.AddConfigPath(configDir)
	viper.SetConfigName(ks.Name)
	viper.SetConfigType("yaml")
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintf(os.Stderr, "Error reading config file: %v\n", err)
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

	rootCmd.PersistentFlags().StringVarP(&outFormat, "format", "f", "text", "text, json, or msgpack")

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
			ks.stopServer()
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
		be_status := fmt.Sprintf("\nService is down, not reachable via %s\n\n", which)

		manifest, err := ks.fetchManifest()
		if err == nil {
			be_status = fmt.Sprintf("\nService is up!\n - URL: %s\n - File: %s\n\n", ks.Url, ks.StateFn)
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

			flagName := strings.ToLower(field.Name)

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

			flagName := strings.ToLower(field.Name)
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
				fmt.Fprintf(os.Stderr, "Error shutting down: %+v\n", err)
			}
		}
	}
}

func (ks *KettlingarService) runBackgroundFunction() bool {
	for _, svc := range ks.services {
		if v, ok := svc.(ServiceWithBackground); ok {
			if err := v.ServiceBackground(ks); err != nil {
				fmt.Fprintf(os.Stderr, "Error daemonizing: %v\n", err)
				os.Exit(ExitBackgroundFailed)
			}
			return true
		}
	}
	return false
}

// Helper to create a Cobra command from a MethodDesc
func (ks *KettlingarService) createRpcCommand(m MethodDesc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   m.Name,
		Short: m.Help, // Used in the command list
		Long:  m.Docs, // Shown when specifically calling 'help <cmd>'
		Run: func(cmd *cobra.Command, args []string) {
			ks.doCall(m, cmd)
		},
	}
	for aName, aType := range m.Args {
		flagName := strings.ToLower(aName)
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
		fmt.Fprintf(os.Stderr, "Failed! Already running at: %s\n", ks.Url)
		os.Exit(ExitAlreadyRunning)
	}

	if !foreground {
		if !ks.runBackgroundFunction() {
			args := append(os.Args[1:], "--foreground")
			newCmd := exec.Command(os.Args[0], args...)
			if err := newCmd.Start(); err != nil {
				fmt.Printf("Error daemonizing: %v\n", err)
				return
			}
			fmt.Printf("%s started in background (PID: %d)\n", ks.Name, newCmd.Process.Pid)
		}
		os.Exit(ExitOK)
	}

	ks.Url = fmt.Sprintf("%s/%s", baseURL, ks.Secret)
	stateData := fmt.Sprintf("%s\n%d", ks.Url, os.Getpid())
	os.WriteFile(statePath, []byte(stateData), 0600)

	srv := &http.Server{Addr: ":" + port, Handler: ks.mux}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	if err := ks.runStartupFunctions(); err != nil {
		fmt.Fprintf(os.Stderr, "Startup Failed! %+v\n", err)
		os.Exit(ExitStartupFailed)
	}

	go func() {
		fmt.Printf("%s listening on %s/%s (PID: %d)\n", ks.Name, baseURL, ks.Secret, os.Getpid())
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	<-stop
	fmt.Println("\nShutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	ks.runShutdownFunctions()
	os.Remove(statePath)
}

func (ks *KettlingarService) stopServer() {
	statePath := ks.getStateFilePath()
	data, err := os.ReadFile(statePath)
	if err != nil {
		fmt.Println("Server not running.")
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		os.Remove(statePath)
		return
	}
	var pid int
	fmt.Sscanf(lines[1], "%d", &pid)
	if process, err := os.FindProcess(pid); err == nil {
		fmt.Printf("Stopping %s (PID: %d)...\n", ks.Name, pid)
		process.Signal(syscall.SIGTERM)
	}
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

	return ks.registry, nil
}

func (ks *KettlingarService) doCall(m MethodDesc, cmd *cobra.Command) {
	params := make(map[string]interface{})
	for aName, aType := range m.Args {

		flagName := strings.ToLower(aName)
		viperKey := fmt.Sprintf("%s.%s", m.Name, flagName)

		// Use viper.GetString to catch Env Vars or Config file entries
		v := viper.GetString(viperKey)

		if v != "" {
			val, err := parseValue(v, aType)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: flag --%s: %v\n", strings.ToLower(aName), err)
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	if m.IsGenerator {
		mimeType := resp.Header.Get("Content-Type")
		buf := make([]byte, 1024*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				printOutput(m, buf[:n], mimeType)
			}
			if err == io.EOF {
				break
			} else if err != nil {
				fmt.Printf("Read error: %v\n", err)
				break
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
