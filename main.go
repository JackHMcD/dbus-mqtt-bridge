package main

import (
    "fmt"
    "log"
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

    // Listen for MPRIS messages
    call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, "type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='/org/mpris/MediaPlayer2'")
    if call.Err != nil {
        log.Fatal(call.Err)
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
