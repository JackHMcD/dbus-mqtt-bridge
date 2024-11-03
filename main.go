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
    "time"
)

var configPath string

func init() {
    flag.StringVar(&configPath, "config", "config.json", "path to the config file")
}

var mqttClient mqtt.Client
var dbusConn *dbus.Conn

type DbusConfig struct {
}

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

var config Config

func logError(err interface{}) {
    fmt.Printf("\n[ERROR]: %v", err)
}

func logInfo(msg interface{}) {
    fmt.Printf("\n[INFO]: %+v", msg)
}

func readConfig() (err error) {
    configFile, err := os.Open(configPath)
    if err != nil {
        return fmt.Errorf("failed to open config file: %w", err)
    }
    defer configFile.Close()

    rawData, err := ioutil.ReadAll(configFile)
    if err != nil {
        return fmt.Errorf("failed to read config file: %w", err)
    }

    err = json.Unmarshal(rawData, &config)
    if err != nil {
        return fmt.Errorf("failed to unmarshal config data: %w", err)
    }

    return nil
}

func initDbus() (err error) {
    dbusConn, err = dbus.SessionBus()
    if err != nil {
        return fmt.Errorf("failed to connect to D-Bus session bus: %w", err)
    }
    return nil
}

func initMqtt() (err error) {
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
    if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
        err = token.Error()
        return fmt.Errorf("failed to connect to MQTT server: %w", err)
    }

    // Subscribe to the topic that will trigger play/pause
    if token := mqttClient.Subscribe("PlayPauseMediaPC", 0, func(client mqtt.Client, msg mqtt.Message) {
        handlePlayPause()
    }); token.Wait() && token.Error() != nil {
        return fmt.Errorf("failed to subscribe to MQTT topic: %w", token.Error())
    }

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
            Mqtt: struct {
                Topic string
            }{
                Topic: "MediaStatus",
            },
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
            mapping.Dbus.Path,
            mapping.Dbus.Interface,
            mapping.Dbus.Sender)
        dbusConn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, matchStr)
    }
    return nil
}

func findMPRISPlayers() ([]string, error) {
    conn, err := dbus.SessionBus()
    if err != nil {
        return nil, fmt.Errorf("failed to connect to session bus: %w", err)
    }

    var names []string
    err = conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus").
        Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
    if err != nil {
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

func findMappingForDbusSignal(signal *dbus.Signal) (mapping MappingStruct, err error) {
    for _, mapping = range config.Mapping {
        if signal.Path == mapping.Dbus.Path {
            return mapping, nil
        }
    }
    return MappingStruct{}, errors.New("couldn't find mapping")
}

func getVarFromDbusMsg(msgBody interface{}, structPath string) (value interface{}, err error) {
    parts := strings.Split(structPath, ".")
    for _, part := range parts {
        val := reflect.ValueOf(msgBody)

        if strings.HasPrefix(part, "['") && strings.HasSuffix(part, "']") {
            keyStr := part[2 : len(part)-2]

            var key reflect.Value
            found := false
            for _, key = range val.MapKeys() {
                if key.String() == keyStr {
                    found = true
                    break
                }
            }
            if !found {
                return nil, errors.New("can't find key '" + keyStr + "' in map")
            }
            msgBody = val.MapIndex(key).Interface()
            continue
        }

        if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
            var idx int
            idx, err = strconv.Atoi(part[1 : len(part)-1])
            if err != nil {
                return nil, err
            }
            msgBody = val.Index(idx).Interface()
            continue
        }

        msgBody = val.FieldByName(part).Interface()
    }

    return msgBody, nil
}

func dbusToMqttLoop() {
    signals := make(chan *dbus.Signal, 10)
    dbusConn.Signal(signals)

    for signal := range signals {
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

        token := mqttClient.Publish(mapping.Mqtt.Topic, 0, false, valStr)
        token.Wait()
        if err := token.Error(); err != nil {
            logError(err)
        }
    }
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

    if err := readConfig(); err != nil {
        logError(err)
    }

    if err := initDbus(); err != nil {
        logError(err)
    }

    if err := initMqtt(); err != nil {
	logError(err)
    }

    registerDbusSignals()

    go dbusToMqttLoop()
    go mqttToDbusLoop()

    go func() {
        for {
            registerDbusSignals()
            time.Sleep(10 * time.Second)
        }
    }()

    controlLoop()
}
