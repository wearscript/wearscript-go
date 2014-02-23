package wearscript

import (
	"code.google.com/p/go.net/websocket"
	"fmt"
	"github.com/ugorji/go/codec"
	"strings"
	"sync"
)

func msgpackMarshal(v interface{}) (data []byte, payloadType byte, err error) {
	mh := codec.MsgpackHandle{}
	enc := codec.NewEncoderBytes(&data, &mh)
	err = enc.Encode(v)
	payloadType = websocket.BinaryFrame
	return
}

func rawMarshal(v interface{}) ([]byte, byte, error) {
	return v.([]byte), websocket.BinaryFrame, nil
}

func msgpackConvert(x *[]interface{}) {
	for z := 0; z < len(*x); z++ {
		xv, ok := (*x)[z].([]uint8)
		if ok {
			(*x)[z] = string(xv)
		} else {
			xv2, ok := (*x)[z].([]interface{})
			if ok {
				msgpackConvert(&xv2)
				(*x)[z] = xv2
			}
		}
	}
}

func msgpackUnmarshal(data []byte, payloadType byte, v interface{}) (err error) {
	w := v.(*[]interface{})
	mh := codec.MsgpackHandle{}
	dec := codec.NewDecoderBytes(data, &mh)
	*w = append(*w, data)
	*w = append(*w, &[]interface{}{})
	err = dec.Decode(&((*w)[1]))
	x := ((*w)[1]).(*[]interface{})
	msgpackConvert(x)
	if err != nil {
		fmt.Println(err)
	}
	return
}

type Connection struct {
	ws                 *websocket.Conn
	device_to_channels *map[string][]string
	channels_external  *map[string]bool
	lock               *sync.Mutex
}

type ConnectionManager struct {
	connections                 *[]*Connection
	group, device, group_device string
	channels_internal           map[string]func(string, []byte, []interface{})
	lock                        *sync.Mutex
}

func ConnectionManagerFactory(group, device string) (*ConnectionManager, error) {
	cm := &ConnectionManager{}
	cm.connections = &[]*Connection{}
	cm.group = group
	cm.device = device
	cm.group_device = cm.group + ":" + cm.device
	cm.channels_internal = map[string]func(string, []byte, []interface{}){}
	cm.lock = &sync.Mutex{}
	return cm, nil
}

func (cm *ConnectionManager) Lock() {
	cm.lock.Lock()
}

func (cm *ConnectionManager) Unlock() {
	cm.lock.Unlock()
}

func (cm *ConnectionManager) NewConnection(ws *websocket.Conn) (*Connection, error) {
	conn := &Connection{}
	conn.ws = ws
	conn.device_to_channels = &map[string][]string{}
	conn.channels_external = &map[string]bool{}
	conn.lock = &sync.Mutex{}
	cm.Lock()
	connections := append(*cm.connections, conn)
	cm.connections = &connections
	cm.Unlock()
	fmt.Println(cm.connections)
	msgcodec := websocket.Codec{msgpackMarshal, msgpackUnmarshal}
	fmt.Println("New conn")
	go func() {
		for {
			request := []interface{}{}
			// TODO: How do we kill this off?
			err := msgcodec.Receive(conn.ws, &request)
			if err != nil {
				// On disconnect:
				// 1. Remove from connections
				// 2. Send out empty subscriptions for devices behind it
				fmt.Println("ws: from glass")
				connections = []*Connection{}
				cm.Lock()
				for _, connection := range *cm.connections {
					if connection != conn {
						connections = append(connections, connection)
					}
				}
				cm.connections = &connections
				cm.Unlock()
				for group_device, _ := range *conn.device_to_channels {
					cm.Publish("subscriptions", group_device, []string{})
				}
				break
			}
			dataRaw := request[0].([]byte)
			data := request[1].(*[]interface{})
			channel := (*data)[0].(string)
			fmt.Println("Receive: " + channel)
			//fmt.Println(data)
			if channel == "subscriptions" {
				fmt.Println(data)
				conn.lock.Lock()
				groupDevice := (*data)[1].(string)
				channels := []string{}
				for _, c := range (*data)[2].([]interface{}) {
					channels = append(channels, c.(string))
				}
				(*conn.device_to_channels)[groupDevice] = channels
				// Update channels
				channels_external := &map[string]bool{}
				for _, cs := range *conn.device_to_channels {
					for _, c := range cs {
						(*channels_external)[c] = true
					}
				}
				conn.channels_external = channels_external
				conn.lock.Unlock()

			}
			for _, connSend := range *cm.connections {
				if connSend != conn && connSend.Exists(channel) {
					fmt.Println("Forwarding: " + channel)
					connSend.SendRaw(dataRaw)
				}
			}
			callback := cm.existsInternal(channel)
			if callback != nil {
				callback(channel, dataRaw, *data)
			}
		}
	}()
	// Send this and all other devices to the new client
	conn.Send("subscriptions", cm.group_device, cm.ChannelsInternal())
	for _, connPrev := range *cm.connections {
		for k, v := range *connPrev.device_to_channels {
			if len(v) > 0 {
				conn.Send("subscriptions", k, v)
			}
		}
	}
	return conn, nil
}

func (cm *ConnectionManager) Subscribe(channel string, callback func(string, []byte, []interface{})) {
	cm.Lock()
	publish := cm.channels_internal[channel] == nil
	cm.channels_internal[channel] = callback
	cm.Unlock()
	if publish {
		cm.Publish("subscriptions", cm.group_device, cm.ChannelsInternal())
	}
}

func (conn *Connection) Send(data ...interface{}) {
	msgcodec := websocket.Codec{msgpackMarshal, msgpackUnmarshal}
	err := msgcodec.Send(conn.ws, data)
	if err != nil {
		fmt.Println(err)
	}
}

func (conn *Connection) SendRaw(data []byte) {
	msgcodec := websocket.Codec{rawMarshal, msgpackUnmarshal}
	err := msgcodec.Send(conn.ws, data)
	if err != nil {
		fmt.Println(err)
	}
}

func (cm *ConnectionManager) Unsubscribe(channel string) {
	cm.Lock()
	publish := cm.channels_internal[channel] != nil
	delete(cm.channels_internal, channel)
	cm.Unlock()
	if publish {
		cm.Publish("subscriptions", cm.group_device, cm.ChannelsInternal())
	}
}

func (cm *ConnectionManager) Exists(channel string) bool {
	for _, conn := range *cm.connections {
		if conn.Exists(channel) {
			return true
		}
	}
	return false
}

func (cm *ConnectionManager) existsInternal(channel string) func(string, []byte, []interface{}) {
	splits := strings.Split(channel, ":")
	channelCur := ""
	cm.Lock()
	defer cm.Unlock()
	for _, v := range splits {
		if cm.channels_internal[channelCur] != nil {
			return cm.channels_internal[channelCur]
		}
		if channelCur == "" {
			channelCur += v
		} else {
			channelCur += ":" + v
		}
	}
	return cm.channels_internal[channelCur]
}

func (cm *ConnectionManager) Publish(channel string, data ...interface{}) {
	fmt.Println("Publish: " + channel)
	for _, conn := range *cm.connections {
		if !conn.Exists(channel) {
			continue
		}
		fmt.Println("Publish: " + channel + " Sending")
		conn.Send(append([]interface{}{channel}, data...)...)
	}
}

func (conn *Connection) Exists(channel string) bool {
	if channel == "subscriptions" {
		return true
	}
	splits := strings.Split(channel, ":")
	channelCur := ""
	conn.lock.Lock()
	defer conn.lock.Unlock()
	fmt.Println(*conn.channels_external)
	for _, v := range splits {
		if (*conn.channels_external)[channelCur] {
			return true
		}
		if channelCur == "" {
			channelCur += v
		} else {
			channelCur += ":" + v
		}
	}
	return (*conn.channels_external)[channelCur]
}

func (cm *ConnectionManager) Channel(parts ...string) string {
	return strings.Join(parts, ":")
}

func (cm *ConnectionManager) Subchannel(part string) string {
	// TODO(brandyn): Extend
	return strings.Join([]string{part, cm.group_device}, ":")
}

func (cm *ConnectionManager) Ackchannel(channel string) string {
	return channel + ":ACK"
}

func (cm *ConnectionManager) Group() string {
	return cm.group
}

func (cm *ConnectionManager) Device() string {
	return cm.device
}

func (cm *ConnectionManager) GroupDevice() string {
	return cm.group_device
}

func (cm *ConnectionManager) ChannelsInternal() []string {
	cm.Lock()
	out := make([]string, 0, len(cm.channels_internal))
	for k, _ := range cm.channels_internal {
		out = append(out, k)
	}
	cm.Unlock()
	return out
}

func (cm *ConnectionManager) ChannelsExternal() map[string][]string {
	channelsExternal := map[string][]string{}
	for _, conn := range *cm.connections {
		for k, v := range conn.ChannelsExternal() {
			channelsExternal[k] = v
		}
	}
	return channelsExternal
}

func (conn *Connection) ChannelsExternal() map[string][]string {
	return *conn.device_to_channels
}

func (cm *ConnectionManager) SubscribeTestHandler() {
	var callback func(string, []byte, []interface{})
	callback = func(channel string, dataRaw []byte, data []interface{}) {
		command, ok := data[1].(string)
		if !ok {
			return
		}
		channelArg, ok := data[2].(string)
		if !ok {
			return
		}
		switch command {
		case "subscribe":
			cm.Subscribe(channelArg, callback)
		case "unsubscribe":
			cm.Unsubscribe(channelArg)
		case "channelsInternal":
			cm.Publish(channelArg, cm.ChannelsInternal())
		case "channelsExternal":
			cm.Publish(channelArg, cm.ChannelsExternal())
		case "group":
			cm.Publish(channelArg, cm.Group())
		case "device":
			cm.Publish(channelArg, cm.Device())
		case "groupDevice":
			cm.Publish(channelArg, cm.GroupDevice())
		case "exists":
			channelExists, ok := data[3].(string)
			if !ok {
				fmt.Println("TestError[exists]: bad channel")
				return
			}
			cm.Publish(channelArg, cm.Exists(channelExists))
		case "publish":
			cm.Publish(channelArg, data[3:]...)
		case "channel":
			channelParts := []string{}
			for _, v := range data[3:] {
				channelPart, ok := v.(string)
				if !ok {
					fmt.Println("TestError[channel]: bad part")
					return
				}
				channelParts = append(channelParts, channelPart)
			}
			cm.Publish(channelArg, cm.Channel(channelParts...))
		case "subchannel":
			channelParam, ok := data[3].(string)
			if !ok {
				fmt.Println("TestError[subchannel]: bad param")
				return
			}
			cm.Publish(channelArg, cm.Subchannel(channelParam))
		case "ackchannel":
			channelParam, ok := data[3].(string)
			if !ok {
				fmt.Println("TestError[subchannel]: bad param")
				return
			}
			cm.Publish(channelArg, cm.Ackchannel(channelParam))
		}
	}
	cm.Subscribe("test:"+cm.group_device, callback)
}
