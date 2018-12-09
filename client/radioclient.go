package client

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/racerxdl/radioserver/protocol"
	"log"
	"net"
	"time"
)

func min(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

var softwareName = "RadioClient"

func SetSoftwareName(name string) {
	softwareName = name
}

// RadioClient connection handler.
// Use MakeRadioClient or MakeRadioClientFullHS to create an instance.
type RadioClient struct {
	fullhostname string
	callback     Callback
	client       net.Conn

	terminated     bool
	routineRunning bool
	gotDeviceInfo  bool
	gotSyncInfo    bool
	streamingMode  uint32
	gain           uint32

	availableSampleRates []uint32

	parserPhase        uint32
	deviceInfo         protocol.DeviceInfo
	header             protocol.MessageHeader
	lastSequenceNumber uint32
	droppedBuffers     uint32
	downStreamBytes    uint64
	parserPosition     uint32
	bodyBuffer         []uint8
	headerBuffer       []uint8

	Streaming      bool
	CanControl     bool
	IsConnected    bool
	DroppedBuffers uint32

	MinimumTunableFrequency uint32
	MaximumTunableFrequency uint32
	DeviceCenterFrequency   uint32
	channelCenterFrequency  uint32
	SmartCenterFrequency    uint32

	currentSampleRate      uint32
	currentSmartSampleRate uint32
	channelDecimation      uint32
	smartDecimation        uint32
}

// MakeRadioClientByFullHS creates an instance of RadioClient by giving hostname + port.
func MakeRadioClientByFullHS(fullhostname string) *RadioClient {
	var s = &RadioClient{
		fullhostname:         fullhostname,
		callback:             nil,
		terminated:           false,
		gotDeviceInfo:        false,
		gotSyncInfo:          false,
		parserPhase:          protocol.ParserAcquiringHeader,
		Streaming:            false,
		CanControl:           false,
		IsConnected:          false,
		availableSampleRates: []uint32{},
		headerBuffer:         make([]uint8, protocol.MessageHeaderSize),
		streamingMode:        protocol.StreamTypeIQ,
		smartDecimation:      1,
	}
	s.cleanup()
	return s
}

// MakeRadioClient creates an instance of RadioClient by giving hostname and port as separated parameters.
func MakeRadioClient(hostname string, port int) *RadioClient {
	var s = &RadioClient{
		fullhostname:         fmt.Sprintf("%s:%d", hostname, port),
		callback:             nil,
		terminated:           false,
		gotDeviceInfo:        false,
		gotSyncInfo:          false,
		parserPhase:          protocol.ParserAcquiringHeader,
		Streaming:            false,
		CanControl:           false,
		IsConnected:          false,
		availableSampleRates: []uint32{},
		headerBuffer:         make([]uint8, protocol.MessageHeaderSize),
		streamingMode:        protocol.StreamTypeIQ,
		smartDecimation:      1,
	}
	s.cleanup()
	return s
}

// region Private Methods

// sayHello sends a Hello Command to the server, with the Software ID (in this case, spy2go)
func (f *RadioClient) sayHello() bool {
	var softwareVersionBytes = []byte(softwareName)

	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, protocol.CurrentProtocolVersion.ToUint64())
	_ = binary.Write(buf, binary.LittleEndian, softwareVersionBytes)

	return f.sendCommand(protocol.CmdHello, buf.Bytes())
}

// cleanup Cleans up all variables and returns to its default states.
func (f *RadioClient) cleanup() {
	f.deviceInfo.DeviceType = protocol.DeviceInvalid
	f.deviceInfo.DeviceSerial = 0
	f.deviceInfo.MaximumSampleRate = 0
	f.deviceInfo.MinimumFrequency = 0
	f.deviceInfo.MaximumFrequency = 0

	f.gain = 0
	f.CanControl = false
	f.gotDeviceInfo = false
	f.gotSyncInfo = false

	f.lastSequenceNumber = 0xFFFFFFFF
	f.droppedBuffers = 0
	f.downStreamBytes = 0
	f.parserPhase = protocol.ParserAcquiringHeader
	f.parserPosition = 0

	f.Streaming = false
	f.terminated = true
}

// onConnect is executed just after a connection is made with RadioClient and got a synchronization info.
// It updates all settings on RadioClient
func (f *RadioClient) onConnect() {
	f.setSetting(protocol.SettingStreamingMode, []uint32{f.streamingMode})
	f.setSetting(protocol.SettingSmartDecimation, []uint32{1})

	var sampleRates = make([]uint32, f.deviceInfo.DecimationStages)
	for i := uint32(0); i < f.deviceInfo.DecimationStages; i++ {
		var decim = uint32(1 << i)
		sampleRates[i] = uint32(float32(f.deviceInfo.MaximumSampleRate) / float32(decim))
	}
	f.availableSampleRates = sampleRates
}

// setSetting changes a setting in RadioClient
func (f *RadioClient) setSetting(settingType uint32, params []uint32) bool {
	var argBytes = make([]uint8, 0)

	if len(params) > 0 {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.LittleEndian, settingType)
		for i := 0; i < len(params); i++ {
			_ = binary.Write(buf, binary.LittleEndian, params[i])
		}
		argBytes = buf.Bytes()
	}

	return f.sendCommand(protocol.CmdSetSetting, argBytes)
}

// sendCommand sends a command to RadioClient
func (f *RadioClient) sendCommand(cmd uint32, args []uint8) bool {
	if f.client == nil {
		return false
	}

	var argsLen = uint32(0)
	if args != nil {
		argsLen += uint32(len(args))
	}

	var buff = new(bytes.Buffer)

	var header = protocol.CommandHeader{
		CommandType: cmd,
		BodySize:    argsLen,
	}

	err := binary.Write(buff, binary.LittleEndian, &header)
	if err != nil {
		panic(err)
	}

	if args != nil {
		for i := 0; i < len(args); i++ {
			buff.WriteByte(byte(args[i]))
		}
	}

	_, err = f.client.Write(buff.Bytes())
	if err != nil {
		fmt.Println(err)
		return false
	}

	return true
}

func (f *RadioClient) parseMessage(buffer []uint8) {
	f.downStreamBytes++

	var consumed uint32
	for len(buffer) > 0 && !f.terminated {
		if f.parserPhase == protocol.ParserAcquiringHeader {
			for f.parserPhase == protocol.ParserReadingData && len(buffer) > 0 {
				consumed = f.parseHeader(buffer)
				buffer = buffer[consumed:]
			}

			if f.parserPhase == protocol.ParserReadingData {
				clientMajor := protocol.CurrentProtocolVersion.Major
				clientMinor := protocol.CurrentProtocolVersion.Minor

				serverVersion := protocol.SplitProtocolVersion(f.header.ProtocolVersion)

				if clientMajor != serverVersion.Major || clientMinor != serverVersion.Minor {
					panic("Server is running an unsupported protocol version.")
				}

				if f.header.BodySize > protocol.MaxMessageBodySize {
					panic("Server sent more than expected body size.")
				}

				f.bodyBuffer = make([]uint8, f.header.BodySize)
			}
		}

		if f.parserPhase == protocol.ParserReadingData {
			consumed = f.parseBody(buffer)
			buffer = buffer[consumed:]

			if f.parserPhase == protocol.ParserAcquiringHeader {
				gap := f.header.PacketNumber - f.lastSequenceNumber - 1
				f.lastSequenceNumber = f.header.PacketNumber
				f.droppedBuffers += gap
				if gap > 0 {
					log.Printf("Lost %d packets from Radio Server!\n", gap)
				}
				f.handleNewMessage()
			}
		}
	}
}

func (f *RadioClient) parseHeader(buffer []uint8) uint32 {
	consumed := uint32(0)

	for len(buffer) > 0 {
		toWrite := min(protocol.MessageHeaderSize-f.parserPosition, uint32(len(buffer)))
		for i := uint32(0); i < toWrite; i++ {
			f.headerBuffer[i+f.parserPosition] = buffer[i]
		}
		buffer = buffer[toWrite:]
		consumed += toWrite
		f.parserPosition += toWrite

		if f.parserPosition == protocol.MessageHeaderSize {
			f.parserPosition = 0
			buf := bytes.NewReader(f.headerBuffer)
			err := binary.Read(buf, binary.LittleEndian, &f.header)
			if err != nil {
				panic(err)
			}

			if f.header.BodySize > 0 {
				f.parserPhase = protocol.ParserReadingData
			}

			return consumed
		}
	}

	return consumed
}

func (f *RadioClient) parseBody(buffer []uint8) uint32 {
	consumed := uint32(0)

	for len(buffer) > 0 {
		toWrite := min(f.header.BodySize-f.parserPosition, uint32(len(buffer)))
		for i := uint32(0); i < toWrite; i++ {
			f.bodyBuffer[i+f.parserPosition] = buffer[i]
		}
		buffer = buffer[toWrite:]
		consumed += toWrite
		f.parserPosition += toWrite

		if f.parserPosition == f.header.BodySize {
			f.parserPosition = 0
			f.parserPhase = protocol.ParserAcquiringHeader
			return consumed
		}
	}

	return consumed
}

func (f *RadioClient) processDeviceInfo() {
	var dInfo = protocol.DeviceInfo{}

	buf := bytes.NewReader(f.bodyBuffer)
	err := binary.Read(buf, binary.LittleEndian, &dInfo)
	if err != nil {
		panic(err)
	}

	f.deviceInfo = dInfo
	f.gotDeviceInfo = true
}

func (f *RadioClient) processClientSync() {
	var clientSync = protocol.ClientSync{}

	buf := bytes.NewReader(f.bodyBuffer)
	err := binary.Read(buf, binary.LittleEndian, &clientSync)
	if err != nil {
		panic(err)
	}

	f.CanControl = clientSync.AllowControl != 0
	f.gain = clientSync.Gains[0]
	f.DeviceCenterFrequency = clientSync.DeviceCenterFrequency
	f.SmartCenterFrequency = clientSync.SmartCenterFrequency

	if f.streamingMode == protocol.StreamTypeCombined || f.streamingMode == protocol.StreamTypeSmartIQ {
		f.MinimumTunableFrequency = clientSync.MinimumSmartFrequency
		f.MaximumTunableFrequency = clientSync.MaximumSmartFrequency
	} else if f.streamingMode == protocol.StreamTypeIQ {
		f.MinimumTunableFrequency = clientSync.MinimumIQCenterFrequency
		f.MaximumTunableFrequency = clientSync.MaximumIQCenterFrequency
	}

	f.gotSyncInfo = true

	if f.callback != nil {
		f.callback.OnData(protocol.MsgTypeClientSync, nil)
	}
}

func (f *RadioClient) processIQ() {
	var sampleCount = f.header.BodySize / 4
	if f.callback != nil {
		var c16arr = make([]ComplexInt16, sampleCount)
		buf := bytes.NewBuffer(f.bodyBuffer)

		var tmp = make([]int16, sampleCount*2)
		_ = binary.Read(buf, binary.LittleEndian, &tmp)

		for i := uint32(0); i < sampleCount; i++ {
			c16arr[i] = ComplexInt16{
				Real: tmp[i*2],
				Imag: tmp[i*2+1],
			}
		}
		f.callback.OnData(SamplesComplex32, c16arr)
	}
}

func (f *RadioClient) processReadSetting() {
	// TODO
}

func (f *RadioClient) processSmartIQ() {
	var sampleCount = f.header.BodySize / 4
	if f.callback != nil {
		var c16arr = make([]ComplexInt16, sampleCount)
		buf := bytes.NewBuffer(f.bodyBuffer)

		var tmp = make([]int16, sampleCount*2)
		_ = binary.Read(buf, binary.LittleEndian, &tmp)

		for i := uint32(0); i < sampleCount; i++ {
			c16arr[i] = ComplexInt16{
				Real: tmp[i*2],
				Imag: tmp[i*2+1],
			}
		}
		f.callback.OnData(SmartSamplesComplex32, c16arr)
	}
}

func (f *RadioClient) handleNewMessage() {
	if f.terminated {
		return
	}

	switch f.header.MessageType {
	case protocol.MsgTypeDeviceInfo:
		f.processDeviceInfo()
	case protocol.MsgTypeClientSync:
		f.processClientSync()
	case protocol.MsgTypeIQ:
		f.processIQ()
	case protocol.MsgTypeSmartIQ:
		f.processSmartIQ()
	case protocol.MsgTypeReadSetting:
		f.processReadSetting()
	}
}

func (f *RadioClient) setStreamState() bool {
	if f.Streaming {
		return f.setSetting(protocol.SettingStreamingEnabled, []uint32{1})
	} else {
		return f.setSetting(protocol.SettingStreamingEnabled, []uint32{0})
	}
}

func (f *RadioClient) threadLoop() {
	f.parserPhase = protocol.ParserAcquiringHeader
	f.parserPosition = 0

	buffer := make([]uint8, 64*1024)

	for f.routineRunning && !f.terminated {
		if f.terminated || !f.routineRunning {
			break
		}

		n, err := f.client.Read(buffer)

		if err != nil {
			if f.routineRunning && !f.terminated {
				log.Println("Error receiving data: ", err)
			}
			break
		}
		if n > 0 {
			var sl = buffer[:n]
			f.parseMessage(sl)
		}
	}
	log.Println("Thread closing")
	f.routineRunning = false
	f.cleanup()
}

// endregion
// region Public Methods

// GetName returns the name of the active device in RadioClient
func (f *RadioClient) GetName() string {
	return protocol.DeviceName[f.deviceInfo.DeviceType]
}

// Start starts the streaming process (if not already started)
func (f *RadioClient) Start() {
	if !f.Streaming {
		log.Println("Starting streaming")
		f.Streaming = true
		f.downStreamBytes = 0
		f.setStreamState()
	}
}

// Stop stop the streaming process (if started)
func (f *RadioClient) Stop() {
	if f.Streaming {
		f.Streaming = false
		f.downStreamBytes = 0
		f.setStreamState()
	}
}

// Connect initiates the connection with RadioClient.
// It panics if the connection fails for some reason.
func (f *RadioClient) Connect() {
	if f.routineRunning {
		return
	}

	log.Println("Trying to connect")
	conn, err := net.Dial("tcp", f.fullhostname)
	if err != nil {
		panic(err)
	}

	f.client = conn
	f.IsConnected = true

	f.sayHello()
	f.cleanup()

	f.terminated = false
	f.gotSyncInfo = false
	f.gotDeviceInfo = false
	f.routineRunning = true

	hasError := false
	errorMsg := ""

	go f.threadLoop()
	log.Println("Connected. Waiting for device info.")
	for i := 0; i < 1000 && !hasError; i++ {
		if f.gotDeviceInfo {
			if f.deviceInfo.DeviceType == protocol.DeviceInvalid {
				errorMsg = "Server is up but no device is available"
				hasError = true
				break
			}

			if f.gotSyncInfo {
				f.onConnect()
				return
			}
		}
		time.Sleep(4 * time.Millisecond)
	}

	f.Disconnect()
	if hasError {
		panic(errorMsg)
	}

	panic("Server didn't send the device capability and synchronization info.")
}

// Disconnect disconnects from current connected RadioClient.
func (f *RadioClient) Disconnect() {
	log.Println("Disconnecting")
	f.terminated = true
	if f.IsConnected {
		_ = f.client.Close()
	}

	f.routineRunning = false
	f.cleanup()
}

// GetSampleRate returns the sample rate of the IQ channel in Hertz
func (f *RadioClient) GetSampleRate() uint32 {
	return f.currentSampleRate
}

// SetSampleRate sets the sample rate of the IQ Channel in Hertz
// Check the available sample rates using GetAvailableSampleRates
// Returns Invalid in case of a invalid value in the input
func (f *RadioClient) SetSampleRate(sampleRate uint32) uint32 {
	for i := uint32(0); i < f.deviceInfo.DecimationStages; i++ {
		if f.availableSampleRates[i] == sampleRate {
			f.channelDecimation = i
			f.setSetting(protocol.SettingIqDecimation, []uint32{i})
			f.currentSampleRate = sampleRate
			if (f.streamingMode == protocol.StreamTypeSmartIQ || f.streamingMode == protocol.StreamTypeCombined) && f.currentSmartSampleRate == 0 {
				f.SetSmartSampleRate(sampleRate)
			}
			return sampleRate
		}
	}

	return protocol.Invalid
}

// SetDecimationStage sets the sample rate by using the number of decimation stages.
// Each decimation stage decimates by two, then the total decimation will be defined by 2^stages.
// This is the same as SetSampleRate, but SetSampleRate instead, looks at a pre-filled table of all 2^stages
// decimations that the server supports and applies into the original device sample rate.
func (f *RadioClient) SetDecimationStage(decimation uint32) uint32 {
	if decimation > f.deviceInfo.DecimationStages {
		return protocol.Invalid
	}
	f.channelDecimation = decimation
	f.setSetting(protocol.SettingIqDecimation, []uint32{decimation})
	f.currentSampleRate = f.availableSampleRates[decimation]

	return decimation
}

// GetCenterFrequency returns the IQ Channel Center Frequency in Hz
func (f *RadioClient) GetCenterFrequency() uint32 {
	return f.channelCenterFrequency
}

// SetCenterFrequency sets the IQ Channel Center Frequency in Hertz and returns it.
func (f *RadioClient) SetCenterFrequency(centerFrequency uint32) uint32 {
	if f.channelCenterFrequency != centerFrequency {
		f.setSetting(protocol.SettingIqFrequency, []uint32{centerFrequency})
		f.channelCenterFrequency = centerFrequency
		if (f.streamingMode == protocol.StreamTypeSmartIQ || f.streamingMode == protocol.StreamTypeCombined) && f.SmartCenterFrequency == 0 {
			f.SetSmartCenterFrequency(centerFrequency)
		}
	}

	return f.channelCenterFrequency
}

// GetDisplayCenterFrequency returns the Smart IQ Center Frequency in Hertz
func (f *RadioClient) GetDisplayCenterFrequency() uint32 {
	return f.SmartCenterFrequency
}

// SetSmartCenterFrequency sets the Smart IQ Center Frequency in Hertz and returns it.
func (f *RadioClient) SetSmartCenterFrequency(centerFrequency uint32) uint32 {
	if f.SmartCenterFrequency != centerFrequency {
		f.setSetting(protocol.SettingSmartFrequency, []uint32{centerFrequency})
		f.SmartCenterFrequency = centerFrequency
	}

	return f.SmartCenterFrequency
}

// SetStreamingMode sets the streaming mode of the server.
// The valid values are StreamTypeSmartIQ, StreamTypeIQ, StreamTypeCombined
func (f *RadioClient) SetStreamingMode(streamMode uint32) {
	if f.streamingMode != streamMode {
		f.streamingMode = streamMode
		f.setSetting(protocol.SettingStreamingMode, []uint32{streamMode})

		if f.streamingMode == protocol.StreamTypeSmartIQ || f.streamingMode == protocol.StreamTypeCombined {
			if f.SmartCenterFrequency == 0 {
				f.SetSmartCenterFrequency(f.GetCenterFrequency())
			}
			f.setSetting(protocol.SettingSmartDecimation, []uint32{f.smartDecimation})
		}
	}
}

// GetStreamingMode returns the streaming mode of the server.
func (f *RadioClient) GetStreamingMode() uint32 {
	return f.streamingMode
}

// SetCallback sets the callbacks for server data
func (f *RadioClient) SetCallback(cb Callback) {
	f.callback = cb
}

// GetAvailableSampleRates returns a list of available sample rates for the current connection.
func (f *RadioClient) GetAvailableSampleRates() []uint32 {
	return f.availableSampleRates
}

// SetSmartSampleRate sets the sample rate of the SmartIQ Channel in Hertz
// Check the available sample rates using GetAvailableSampleRates
// Returns Invalid in case of a invalid value in the input
func (f *RadioClient) SetSmartSampleRate(sampleRate uint32) uint32 {
	for i := uint32(0); i < f.deviceInfo.DecimationStages; i++ {
		if f.availableSampleRates[i] == sampleRate {
			f.smartDecimation = i
			f.setSetting(protocol.SettingSmartDecimation, []uint32{i})
			f.currentSmartSampleRate = sampleRate
			return sampleRate
		}
	}

	return protocol.Invalid
}

// SetSmartDecimation sets the sample rate of the Smart IQ by using the number of decimation stages.
// Each decimation stage decimates by two, then the total decimation will be defined by 2^stages.
// This is the same as SetSampleRate, but SetSampleRate instead, looks at a pre-filled table of all 2^stages
// decimations that the server supports and applies into the original device sample rate.
// Returns Invalid in case of a invalid value in the input
func (f *RadioClient) SetSmartDecimation(decimation uint32) uint32 {
	if decimation > f.deviceInfo.DecimationStages {
		return protocol.Invalid
	}
	f.smartDecimation = decimation
	f.setSetting(protocol.SettingSmartDecimation, []uint32{decimation})
	f.currentSmartSampleRate = f.availableSampleRates[decimation]

	return decimation
}

// GetDisplaySampleRate returns the sample rate of Smart IQ in Hertz
func (f *RadioClient) GetDisplaySampleRate() uint32 {
	return f.currentSmartSampleRate
}

// SetGain sets the gain stage of the server.
// The actual gain in dB varies from device to device.
// Returns Invalid in case of a invalid value in the input
func (f *RadioClient) SetGain(gain uint32) uint32 {
	if gain > f.deviceInfo.MaximumGainValue {
		return protocol.Invalid
	}
	f.setSetting(protocol.SettingGains, []uint32{gain, 0, 0})
	f.gain = gain

	return gain
}

// GetGain returns the current gain stage of the server.
func (f *RadioClient) GetGain() uint32 {
	return f.gain
}

// endregion