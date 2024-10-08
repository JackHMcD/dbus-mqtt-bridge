package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/godbus/dbus"
	"io/ioutil"
	"os"
	"reflect"
	"strconv"
	"strings"
	"flag"
)

var configPath string

func init() {
    flag.StringVar(&configPath, "config", "config.json", "path to the config file")
}

var mqtt_client mqtt.Client
var dbus_conn *dbus.Conn

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
		Topic string // The MQTT-Topicstring to subscribe or to send the commands to
	}
	Dbus struct {
		Type       string
		Path       dbus.ObjectPath
		Interface  string
		Sender     string
		StructPath string
		RemoveQuotmark bool
	}
	Mode string // possible modes: passtrough
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

func logDebug(msg interface{}) {
	fmt.Printf("\n[DEBUG]: %+v", msg)
}

func readConfig() (err error) {
	configFile, err := os.Open(configPath)
	if err != nil {
		return
	}

	rawData, err := ioutil.ReadAll(configFile)
	if err != nil {
		return
	}

	err = json.Unmarshal(rawData, &config)
	if err != nil {
		return
	}
	logDebug(config)
	return
}

func initDbus() (err error) {
	dbus_conn, err = dbus.SessionBus()
	if err != nil {
		return
	}
	return
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

	mqtt_client = mqtt.NewClient(opts)
	if token := mqtt_client.Connect(); token.Wait() && token.Error() != nil {
		err = token.Error()
		return
	}
	return
}

func registerDbusSignals() (error) {
	for _, mapping := range config.Mapping {
		logDebug(mapping)
		matchStr := fmt.Sprintf("type='signal',path='%v',interface='%v',sender='%v'",
			mapping.Dbus.Path,
			mapping.Dbus.Interface,
			findMPRISPlayers())
		dbus_conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, matchStr)
	}
	return nil
}

func findMPRISPlayers() string {
    // Connect to the session bus
    conn, err := dbus.SessionBus()
    if err != nil {
        return fmt.Sprintf("Failed to connect to session bus: %v", err)
    }

    // Call ListNames method on the bus
    var names []string
    err = conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus").
        Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
    if err != nil {
        return fmt.Sprintf("Failed to list names on the bus: %v", err)
    }

    // Filter names to find MPRIS players
    var result strings.Builder
    for _, name := range names {
        if strings.HasPrefix(name, "org.mpris.MediaPlayer2") {
            result.WriteString(name)
        }
    }

    return result.String()
}

func findMappingForDbusSignal(signal *dbus.Signal) (mapping MappingStruct, err error) {
	for _, mapping = range config.Mapping {
		if signal.Path == mapping.Dbus.Path { // &&
			//signal.Sender == mapping.Dbus.Sender { // ToDo: sender from signal has not the expected value
			return
		}
	}
	mapping = MappingStruct{}
	err = errors.New("couldn't find mapping")
	return
}

func getVarFromDbusMsg(msgBody interface{}, structPath string) (value interface{}, err error) {
	parts := strings.Split(structPath, ".")
	for _, part := range parts {
		val := reflect.ValueOf(msgBody)
		fmt.Printf("Current msgBody: %+v\n", msgBody) // Debugging line

		// check, if next part is a map
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
//				err = errors.New("can't find key '" + keyStr + "' in map")
				return
			}
			msgBody = val.MapIndex(key).Interface()
			continue
		}

		// check, if next part is a slice
		if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
			var idx int
			idx, err = strconv.Atoi(part[1 : len(part)-1])
			if err != nil {
				return
			}
			msgBody = val.Index(idx).Interface()
			continue
		}

		// if neither map or slice, it must be a struct
		msgBody = val.FieldByName(part).Interface()
	}

		value = msgBody

	return
}





func dbus_to_mqtt_loop() {
	signals := make(chan *dbus.Signal, 10)
	dbus_conn.Signal(signals)

	for signal := range signals {
		// get corresponding mapping
		mapping, err := findMappingForDbusSignal(signal)
		if err != nil {
			logError(err)
			continue
		}

		// search for the variable using the structPath
		var value interface{}
		value, err = getVarFromDbusMsg(signal.Body, mapping.Dbus.StructPath)
		if err != nil {
			logError(err)
		}
		valStr := fmt.Sprint(value)
		if mapping.Dbus.RemoveQuotmark {
			valStr = strings.Replace(valStr, "\"", "", -1)
		}

		if strings.Compare(valStr, "<nil>") == 0 {
			continue
		}
		
		// send MQTT-Message
		token := mqtt_client.Publish(mapping.Mqtt.Topic, 0, false, valStr)
		token.Wait()
		err = token.Error()
		if err != nil {
			logError(err)
		}
	}
}

func mqtt_to_dbus_loop() {

}

func ctrl_loop() {
	reader := bufio.NewReader(os.Stdin)
	exit := false

	for !exit {
		fmt.Print("# ")

		cmd, _ := reader.ReadString('\n')
		cmd = cmd[0 : len(cmd)-1]

		switch cmd {
		case "exit":
			exit = true
		}
	}
}

func main() {
	flag.Parse()
    	fmt.Println("Config file path:", configPath)
	
	var err error
	err = readConfig()
	if err != nil {
		logError(err)
	}

	initDbus()
	initMqtt()

	registerDbusSignals()

	go dbus_to_mqtt_loop()
	go mqtt_to_dbus_loop()

	ctrl_loop()
}
