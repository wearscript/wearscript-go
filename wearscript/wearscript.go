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
	fmt.Println(w)
	err = dec.Decode(&((*w)[1]))
	fmt.Println(w)
	x := ((*w)[1]).(*[]interface{})
	msgpackConvert(x)
	fmt.Println(*x)
	if err != nil {
		fmt.Println(err)
	}
	//fmt.Println("Unmarshal: " + B64Enc(string(data)))
	return
}

type Connection struct {
	ws                 *websocket.Conn
	device_to_channels map[string][]string
	channels_external  map[string]bool
	lock               *sync.Mutex
}

type ConnectionManager struct {
	connections                 []*Connection
	group, device, group_device string
	channels_internal           map[string]func(string, []byte, []interface{})
	lock                        *sync.Mutex
}

func ConnectionManagerFactory(group, device string) (*ConnectionManager, error) {
	cm := &ConnectionManager{}
	cm.connections = []*Connection{}
	cm.group = group
	cm.device = device
	cm.group_device = cm.group + ":" + cm.device
	cm.channels_internal = map[string]func(string, []byte, []interface{}){}
	cm.lock = &sync.Mutex{}
	return cm, nil
}

func (cm *ConnectionManager) NewConnection(ws *websocket.Conn) (*Connection, error) {
	conn := &Connection{}
	conn.ws = ws
	conn.device_to_channels = map[string][]string{}
	conn.channels_external = map[string]bool{}
	conn.lock = &sync.Mutex{}
	cm.connections = append(cm.connections, conn)
	fmt.Println(cm.connections)
	msgcodec := websocket.Codec{msgpackMarshal, msgpackUnmarshal}
	fmt.Println("New conn")
	go func() {
		for {
			fmt.Println("Receive")
			request := []interface{}{}
			// TODO: How do we kill this off?
			err := msgcodec.Receive(conn.ws, &request)
			if err != nil {
				// On disconnect:
				// 1. Remove from connections
				// 2. Send out empty subscriptions for devices behind it
				fmt.Println("ws: from glass")
				connections := []*Connection{}
				for _, connection := range cm.connections {
					if connection != conn {
						connections = append(connections, connection)
					}
				}
				cm.connections = connections
				for group_device, _ := range conn.device_to_channels {
					cm.Publish("subscriptions", group_device, []string{})
				}
				break
			}
			dataRaw := request[0].([]byte)
			data := request[1].(*[]interface{})
			channel := (*data)[0].(string)
			fmt.Println(data)
			if channel == "subscriptions" {
				conn.lock.Lock()
				groupDevice := (*data)[1].(string)
				channels := []string{}
				for _, c := range (*data)[2].([]interface{}) {
					channels = append(channels, c.(string))
				}
				conn.device_to_channels[groupDevice] = channels
				// Update channels
				channels_external := map[string]bool{}
				for _, cs := range conn.device_to_channels {
					for _, c := range cs {
						channels_external[c] = true
					}
				}
				conn.channels_external = channels_external
				fmt.Println(conn.channels_external)
				conn.lock.Unlock()

			}
			for _, connSend := range cm.connections {
				if connSend != conn && connSend.Exists(channel) {
					connSend.SendRaw(dataRaw)
				}
			}
			// BUG(brandyn): This should use the soft matching like Exists
			if cm.channels_internal[channel] != nil {
				cm.channels_internal[channel](channel, dataRaw, *data)
			}
		}
	}()
	// Send this and all other devices to the new client
	if len(cm.channels_internal) > 0 {
		cm.Publish("subscriptions", cm.group_device, cm.ChannelsInternal())
	}
	for _, conn := range cm.connections {
		for k, v := range conn.device_to_channels {
			if len(v) > 0 {
				cm.Publish("subscriptions", k, v)
			}
		}
	}
	fmt.Println("After")
	return conn, nil
}

func (cm *ConnectionManager) Subscribe(channel string, callback func(string, []byte, []interface{})) {
	if cm.channels_internal[channel] == nil {
		cm.channels_internal[channel] = callback
		cm.Publish("subscriptions", cm.group_device, cm.ChannelsInternal())
	} else {
		cm.channels_internal[channel] = callback
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
	if cm.channels_internal[channel] != nil {
		delete(cm.channels_internal, channel)
		cm.Publish("subscriptions", cm.group_device, cm.ChannelsInternal())
	}
}

func (cm *ConnectionManager) Publish(channel string, data ...interface{}) {
	for _, conn := range cm.connections {
		if !conn.Exists(channel) {
			return
		}
		conn.Send(append([]interface{}{channel}, data...)...)
	}
}

func (conn *Connection) Exists(channel string) bool {
	if channel == "subscriptions" {
		return true
	}
	splits := strings.Split(channel, ":")
	channelCur := ""
	fmt.Println(conn.channels_external)
	for _, v := range splits {
		fmt.Println("ExistCheck:" + channelCur)
		if conn.channels_external[channelCur] {
			fmt.Println("Exists: " + channelCur)
			return true
		}
		if channelCur == "" {
			channelCur += v
		} else {
			channelCur += ":" + v
		}
	}
	fmt.Println("ExistCheck:" + channelCur)
	if  conn.channels_external[channelCur] {
		fmt.Println("Exists: " + channelCur)
	}
	return conn.channels_external[channelCur]
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
	out := make([]string, 0, len(cm.channels_internal))
	for k, _ := range cm.channels_internal {
		out = append(out, k)
	}
	return out
}

func (conn *Connection) ChannelsExternal() map[string][]string {
	return conn.device_to_channels
}
