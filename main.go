package main

import (
    "bufio"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    mqtt "github.com/eclipse/paho.mqtt.golang"
    "github.com/godbus/dbus"
    "io/ioutil"
    "os"
    "reflect"
    "strconv"
    "strings"
    "sync"
    "time"
)

var configPath string

func init() {
    flag.StringVar(&configPath, "config", "config.json", "path to the config file")
}

var (
    mqttClient mqtt.Client
    dbusConn   *dbus.Conn
    config     Config
    signalCh   chan *dbus.Signal
    wg         sync.WaitGroup
    debounceMap = make(map[string]time.Time)
    debounceDuration = 500 * time.Millisecond
    bufferedSignalCh = make(chan *dbus.Signal, 100)
)

type DbusConfig struct{}

type MqttConfig struct {
    Servers  []string
    ClientID string
    Username string
    Password string
}

type MappingStruct struct {
    Mqtt struct {
        Topic string
    }
    Dbus struct {
        Type            string
        Path            dbus.ObjectPath
        Interface       string
        Sender          string
        StructPath      string
        RemoveQuotmark  bool
    }
    Mode string
}

type Config struct {
    Dbus    DbusConfig
    Mqtt    MqttConfig
    Mapping []MappingStruct
}

func logError(err error) {
    fmt.Printf("\n[ERROR]: %v", err)
}

func logInfo(msg string) {
    fmt.Printf("\n[INFO]: %s", msg)
}

func readConfig() error {
    configFile, err := os.Open(configPath)
    if err != nil {
        return fmt.Errorf("failed to open config file: %w", err)
    }
    defer configFile.Close()

    rawData, err := ioutil.ReadAll(configFile)
    if err != nil {
        return fmt.Errorf("failed to read config file: %w", err)
    }

    return json.Unmarshal(rawData, &config)
}

func initDbus() error {
    var err error
    dbusConn, err = dbus.SessionBus()
    if err != nil {
        return fmt.Errorf("failed to connect to D-Bus session bus: %w", err)
    }
    return nil
}


func initMqtt() error {
    conf := config.Mqtt

    opts := mqtt.NewClientOptions()
    for _, server := range conf.Servers {
        opts.AddBroker(server)
    }
    opts.SetClientID(conf.ClientID)
    if conf.Username != "" {
        opts.SetUsername(conf.Username)
    }
    if conf.Password != "" {
        opts.SetPassword(conf.Password)
    }

    mqttClient = mqtt.NewClient(opts)
    token := mqttClient.Connect()
    if token.Wait() && token.Error() != nil {
        return fmt.Errorf("failed to connect to MQTT server: %w", token.Error())
    }

    logInfo("Connected to MQTT server")

    // Subscribe to the topic that will trigger play/pause
    token = mqttClient.Subscribe("PlayPauseMediaPC", 0, func(client mqtt.Client, msg mqtt.Message) {
        handlePlayPause()
    })
    if token.Wait() && token.Error() != nil {
        return fmt.Errorf("failed to subscribe to MQTT topic: %w", token.Error())
    }

    logInfo("Subscribed to MQTT topic: PlayPauseMediaPC")
    return nil
}


func handlePlayPause() {
    players, err := findMPRISPlayers()
    if err != nil {
        logError(err)
        return
    }

    for _, player := range players {
        obj := dbusConn.Object(player, "/org/mpris/MediaPlayer2")
        call := obj.Call("org.mpris.MediaPlayer2.Player.PlayPause", 0)
        if call.Err != nil {
            logError(call.Err)
        }
    }
}

func registerDbusSignals() error {
    players, err := findMPRISPlayers()
    if err != nil {
        return fmt.Errorf("failed to find MPRIS players: %w", err)
    }

    for _, player := range players {
        mapping := MappingStruct{
            Mqtt: struct{ Topic string }{Topic: "MediaStatus"},
            Dbus: struct {
                Type            string
                Path            dbus.ObjectPath
                Interface       string
                Sender          string
                StructPath      string
                RemoveQuotmark  bool
            }{
                Type:            "Signal",
                Path:            dbus.ObjectPath("/org/mpris/MediaPlayer2"),
                Interface:       "org.freedesktop.DBus.Properties",
                Sender:          player,
                StructPath:      "[1].['PlaybackStatus']",
                RemoveQuotmark:  true,
            },
            Mode: "passtrough",
        }
        config.Mapping = append(config.Mapping, mapping)

        matchStr := fmt.Sprintf("type='signal',path='%v',interface='%v',sender='%v'",
            mapping.Dbus.Path, mapping.Dbus.Interface, mapping.Dbus.Sender)
        dbusConn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, matchStr)
    }
    return nil
}

func findMPRISPlayers() ([]string, error) {
    var names []string
    if err := dbusConn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus").
        Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
        return nil, fmt.Errorf("failed to list names on the bus: %w", err)
    }

    var players []string
    for _, name := range names {
        if strings.HasPrefix(name, "org.mpris.MediaPlayer2.") {
            players = append(players, name)
        }
    }
    return players, nil
}

func findMappingForDbusSignal(signal *dbus.Signal) (MappingStruct, error) {
    for _, mapping := range config.Mapping {
        if signal.Path == mapping.Dbus.Path {
            return mapping, nil
        }
    }
    return MappingStruct{}, errors.New("couldn't find mapping")
}

func getVarFromDbusMsg(msgBody interface{}, structPath string) (interface{}, error) {
    parts := strings.Split(structPath, ".")
    for _, part := range parts {
        val := reflect.ValueOf(msgBody)
        logInfo(fmt.Sprintf("Current part: %s, value: %v", part, val))

        if strings.HasPrefix(part, "['") && strings.HasSuffix(part, "']") {
            keyStr := part[2 : len(part)-2]
            logInfo(fmt.Sprintf("Looking for key: %s", keyStr))

            if val.Kind() == reflect.Map {
                mapVal := val.MapIndex(reflect.ValueOf(keyStr))
                if !mapVal.IsValid() {
                    logInfo(fmt.Sprintf("Key '%s' not found in map. Available keys: %v", keyStr, val.MapKeys()))
                    continue  // Skip and continue to next signal
                }
                msgBody = mapVal.Interface()
                continue
            }
            return nil, fmt.Errorf("message body is not a map, can't access key '%s'", keyStr)
        }

        if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
            idx, err := strconv.Atoi(part[1 : len(part)-1])
            if err != nil {
                return nil, err
            }
            if idx >= val.Len() {
                logInfo(fmt.Sprintf("Index %d out of range in array. Full signal: %v", idx, msgBody))
                return nil, fmt.Errorf("index %d out of range in array", idx)
            }
            msgBody = val.Index(idx).Interface()
            continue
        }

        if val.Kind() == reflect.Ptr {
            val = val.Elem()
        }

        fieldVal := val.FieldByName(part)
        if !fieldVal.IsValid() {
            logInfo(fmt.Sprintf("Field '%s' not found. Full signal: %v", part, msgBody))
            return nil, fmt.Errorf("can't find field '%s'", part)
        }
        msgBody = fieldVal.Interface()
    }

    return msgBody, nil
}



// In dbusToMqttLoop function
func dbusToMqttLoop() {
    signals := make(chan *dbus.Signal, 10)
    dbusConn.Signal(signals)
    signalCh = signals

    for signal := range signalCh {
        logInfo(fmt.Sprintf("Received signal: %v", signal))
        bufferedSignalCh <- signal
    }
}

func processBufferedSignals() {
    for signal := range bufferedSignalCh {
        logInfo(fmt.Sprintf("Processing signal: %v", signal))
        mapping, err := findMappingForDbusSignal(signal)
        if err != nil {
            logError(err)
            continue
        }

        value, err := getVarFromDbusMsg(signal.Body, mapping.Dbus.StructPath)
        if err != nil {
            logError(err)
            continue
        }
        valStr := fmt.Sprint(value)
        if mapping.Dbus.RemoveQuotmark {
            valStr = strings.ReplaceAll(valStr, "\"", "")
        }

        if valStr == "<nil>" {
            continue
        }

        publishMQTT(mapping.Mqtt.Topic, valStr)
    }
}

func publishMQTT(topic, message string) {
    retryCount := 3
    for i := 0; i < retryCount; i++ {
        if token := mqttClient.Publish(topic, 0, false, message); token.Wait() && token.Error() != nil {
            logError(token.Error())
            logInfo(fmt.Sprintf("Retrying %d/%d", i+1, retryCount))
            time.Sleep(1 * time.Second)
        } else {
            logInfo("Published successfully")
            return
        }
    }
    logError(errors.New("failed to publish message after retries"))
}





func shouldProcessSignal(signal *dbus.Signal) bool {
    key := fmt.Sprintf("%v-%v", signal.Path, signal.Name)
    now := time.Now()
    if lastTime, ok := debounceMap[key]; ok {
        if now.Sub(lastTime) < debounceDuration {
            return false
        }
    }
    debounceMap[key] = now
    return true
}

func mqttToDbusLoop() {
    // Placeholder for MQTT to D-Bus implementation
}

func controlLoop() {
    reader := bufio.NewReader(os.Stdin)
    exit := false

    for !exit {
        fmt.Print("# ")

        cmd, _ := reader.ReadString('\n')
        cmd = strings.TrimSpace(cmd)

        switch cmd {
        case "exit":
            exit = true
        default:
            logInfo("Unknown command: " + cmd)
        }
    }
}

func main() {
    flag.Parse()
    fmt.Println("Config file path:", configPath)
    fmt.Println("MQTT Servers:", config.Mqtt.Servers)

    if err := readConfig(); err != nil {
        logError(err)
        return
    }

    if err := initDbus(); err != nil {
        logError(err)
        return
    }

    if err := initMqtt(); err != nil {
        logError(err)
        return
    }

    registerDbusSignals()

    wg.Add(3)
    go func() {
        defer wg.Done()
        dbusToMqttLoop()
    }()
    go func() {
        defer wg.Done()
        mqttToDbusLoop()
    }()
    go func() {
        defer wg.Done()
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()

        for range ticker.C {
            registerDbusSignals()
        }
    }()
    go func() {
        defer wg.Done()
        processBufferedSignals()
    }()

    controlLoop()
    close(signalCh)
    close(bufferedSignalCh)
    wg.Wait()
}
