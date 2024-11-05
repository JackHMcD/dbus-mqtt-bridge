package main

import (
    "fmt"
    "log"
    "strings"
    "github.com/eclipse/paho.mqtt.golang"
    "github.com/godbus/dbus"
)

func main() {
    // Set up MQTT client
    opts := mqtt.NewClientOptions().AddBroker("tcp://192.168.0.100:1883")
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

    // Subscribe to MQTT topic
    topic := "mpris/command"
    client.Subscribe(topic, 0, func(client mqtt.Client, msg mqtt.Message) {
        fmt.Println("MQTT message received")

        // Determine the command
        command := string(msg.Payload())

        // List DBus names
        var names []string
        obj := conn.BusObject()
        call := obj.Call("org.freedesktop.DBus.ListNames", 0)
        if call.Err != nil {
            log.Fatal(call.Err)
        }

        call.Store(&names)

        // Find MPRIS interfaces starting with org.mpris.MediaPlayer2.chromium
        for _, name := range names {
            if strings.HasPrefix(name, "org.mpris.MediaPlayer2.chromium") {
                // Call appropriate method based on the command
                playerObj := conn.Object(name, "/org/mpris/MediaPlayer2")
                var playerCall *dbus.Call

                switch command {
                case "playpause":
                    playerCall = playerObj.Call("org.mpris.MediaPlayer2.Player.PlayPause", 0)
                }

                if playerCall != nil && playerCall.Err != nil {
                    fmt.Println("Failed to execute command:", command, playerCall.Err)
                } else {
                    fmt.Println(command, "successfully called on", name)
                }
            }
        }
    })

    // Listen for MPRIS messages (optional, in case you still need this part)
    addMatchCall := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, "type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='/org/mpris/MediaPlayer2'")
    if addMatchCall.Err != nil {
        log.Fatal(addMatchCall.Err)
    }

    c := make(chan *dbus.Signal, 10)
    conn.Signal(c)

    fmt.Println("Listening for MPRIS messages and publishing to MQTT...")

    for v := range c {
        if v.Name == "org.freedesktop.DBus.Properties.PropertiesChanged" {
            fmt.Println("MPRIS message received:", v.Body)
            token := client.Publish("mpris/messages", 0, false, fmt.Sprintf("%v", v.Body))
            token.Wait()
        }
    }
}
