package main

import (
    "fmt"
    "log"
    "github.com/eclipse/paho.mqtt.golang"
    "github.com/godbus/dbus"
    "strings"
)

// Function to discover MPRIS media player service name
func discoverMediaPlayer(conn *dbus.Conn) (string, error) {
    obj := conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")
    var services []string
    err := obj.Call("org.freedesktop.DBus.ListNames", 0).Store(&services)
    if err != nil {
        return "", err
    }
    for _, service := range services {
        if strings.HasPrefix(service, "org.mpris.MediaPlayer2.") {
            return service, nil
        }
    }
    return "", fmt.Errorf("no MPRIS media player service found")
}

func main() {
    // Set up MQTT client
    opts := mqtt.NewClientOptions().AddBroker("tcp://broker.hivemq.com:1883")
    opts.SetClientID("mpris_mqtt_client")
    client := mqtt.NewClient(opts)
    if token := client.Connect(); token.Wait() && token.Error() != nil {
        log.Fatal(token.Error())
    }

    // Set up DBus connection
    conn, err := dbus.SessionBus()
    if err != nil {
        log.Fatal(err)
    }

    // Discover MPRIS media player service name
    mediaPlayer, err := discoverMediaPlayer(conn)
    if err != nil {
        log.Fatal("Failed to discover media player:", err)
    }
    fmt.Println("Discovered media player service:", mediaPlayer)

    // Listen for MPRIS messages
    call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, fmt.Sprintf("type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='/org/mpris/MediaPlayer2',sender='%s'", mediaPlayer))
    if call.Err != nil {
        log.Fatal(call.Err)
    }

    c := make(chan *dbus.Signal, 10)
    conn.Signal(c)

    fmt.Println("Listening for MPRIS messages and publishing to MQTT...")

    // Subscribe to MQTT topic for play/pause commands
    token := client.Subscribe("mpris/commands", 0, func(client mqtt.Client, msg mqtt.Message) {
        command := string(msg.Payload())
        switch command {
        case "play":
            sendCommand(conn, mediaPlayer, "Play")
        case "pause":
            sendCommand(conn, mediaPlayer, "Pause")
        default:
            fmt.Println("Unknown command:", command)
        }
    })
    token.Wait()

    // Publish received MPRIS messages to MQTT
    for v := range c {
        if v.Name == "org.freedesktop.DBus.Properties.PropertiesChanged" {
            fmt.Println("MPRIS message received:", v.Body)
            token := client.Publish("mpris/messages", 0, false, fmt.Sprintf("%v", v.Body))
            token.Wait()
        }
    }
}

// Function to send commands to the media player
func sendCommand(conn *dbus.Conn, mediaPlayer string, command string) {
    obj := conn.Object(mediaPlayer, "/org/mpris/MediaPlayer2")
    call := obj.Call(fmt.Sprintf("org.mpris.MediaPlayer2.Player.%s", command), 0)
    if call.Err != nil {
        fmt.Println("Failed to send command:", call.Err)
    }
}
