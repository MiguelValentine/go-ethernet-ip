package goplc

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/MiguelValentine/goplc/enip/cip"
	"github.com/MiguelValentine/goplc/enip/cip/epath/segment"
	"github.com/MiguelValentine/goplc/enip/cip/messageRouter"
	"github.com/MiguelValentine/goplc/enip/cip/unconnectedSend"
	"github.com/MiguelValentine/goplc/enip/encapsulation"
	"github.com/MiguelValentine/goplc/enip/etype"
	"github.com/MiguelValentine/goplc/enip/lib"
	"io"
	"math/rand"
	"net"
	"time"
)

type controller struct {
	VendorID     etype.XUINT
	DeviceType   etype.XUINT
	ProductCode  etype.XUINT
	Major        uint8
	Minor        uint8
	Status       uint16
	SerialNumber uint32
	Version      string
	Name         string
}

type plc struct {
	tcpAddr    *net.TCPAddr
	tcpConn    *net.TCPConn
	config     *Config
	sender     chan []byte
	context    uint64
	session    etype.XUDINT
	slot       uint8
	request    *encapsulation.Request
	path       []byte
	Controller *controller
}

func (p *plc) Connect() error {
	p.config.Println("Connecting...")
	_conn, err := net.DialTCP("tcp", nil, p.tcpAddr)
	if err != nil {
		return err
	}

	err2 := _conn.SetKeepAlive(true)
	if err2 != nil {
		return err2
	}

	p.tcpConn = _conn
	p.connected()
	return nil
}

func (p *plc) connected() {
	if p.config.OnConnected != nil {
		p.config.OnConnected()
	}

	p.config.Println("PLC Connected!")
	p.config.EBF.Clean()

	go p.read()
	go p.write()

	p.registerSession()
}

func (p *plc) registerSession() {
	p.config.Println("Register Session")
	p.sender <- p.request.RegisterSession(p.context)
}

func (p *plc) readControllerProps() {
	mr := messageRouter.Build(messageRouter.ServiceGetAttributeAll, [][]byte{
		segment.LogicalBuild(segment.LogicalTypeClassID, 0x01, true),
		segment.LogicalBuild(segment.LogicalTypeInstanceID, 0x01, true),
	}, nil)
	ucmm := unconnectedSend.Build(mr, p.path, 2000)
	p.sender <- p.request.SendRRData(p.context, p.session, 10, ucmm)
}

func (p *plc) disconnected(err error) {
	if p.config.OnDisconnected != nil {
		p.config.OnDisconnected(err)
	}

	if err != io.EOF {
		p.config.Println("PLC Disconnected!")
		p.config.Println("EOF")
	} else {
		p.config.Println("PLC Disconnected!")
		p.config.Println(err)
	}

	_ = p.tcpConn.Close()
	p.tcpConn = nil

	if p.config.ReconnectionInterval != 0 {
		p.config.Println("Reconnecting...")
		time.Sleep(p.config.ReconnectionInterval)
		err := p.Connect()
		if err != nil {
			panic(err)
		}
	}
}

func (p *plc) write() {
	for {
		select {
		case data := <-p.sender:
			_, _ = p.tcpConn.Write(data)
		}
	}
}

func (p *plc) read() {
	buf := make([]byte, 1024*64)
	for {
		length, err := p.tcpConn.Read(buf)
		if err != nil {
			p.disconnected(err)
			break
		}

		err = p.config.EBF.Read(buf[0:length], p.encapsulationHandle)
		if err != nil {
			p.disconnected(err)
			break
		}
	}
}

func (p *plc) encapsulationHandle(_encapsulation *encapsulation.Encapsulation) {
	switch _encapsulation.Command {
	case encapsulation.CommandRegisterSession:
		p.session = _encapsulation.SessionHandle
		p.config.Printf("session=> %d\n", p.session)
		if p.config.OnRegistered != nil {
			p.config.OnRegistered()
		}
		p.readControllerProps()
	case encapsulation.CommandUnRegisterSession:
		p.disconnected(errors.New("UnRegisterSession"))
	case encapsulation.CommandSendRRData:
		p.config.Printf("SendRRData=> %d\n", _encapsulation.Length)
		_, _cpf := cip.Parser(_encapsulation.Data)
		p.sendRRDataHandle(_cpf)
	case encapsulation.CommandSendUnitData:
	}
}

func (p *plc) sendRRDataHandle(cpf *cip.CPF) {
	mr := messageRouter.Parse(cpf.Items[1].Data)

	if mr.GeneralStatus != 0 {
		p.disconnected(errors.New(string(mr.AdditionalStatus)))
		return
	}
	switch mr.Service - 0x80 {
	case messageRouter.ServiceGetAttributeAll:
		p.getAttributeAllHandle(mr.ResponseData)
	}
}

func (p *plc) getAttributeAllHandle(data []byte) {
	dataReader := bytes.NewReader(data)
	lib.ReadByte(dataReader, &p.Controller.VendorID)
	lib.ReadByte(dataReader, &p.Controller.DeviceType)
	lib.ReadByte(dataReader, &p.Controller.ProductCode)
	lib.ReadByte(dataReader, &p.Controller.Major)
	lib.ReadByte(dataReader, &p.Controller.Minor)
	lib.ReadByte(dataReader, &p.Controller.Status)
	lib.ReadByte(dataReader, &p.Controller.SerialNumber)
	nameLen := uint8(0)
	lib.ReadByte(dataReader, &nameLen)
	nameBuf := make([]byte, nameLen)
	lib.ReadByte(dataReader, nameBuf)

	p.Controller.Name = string(nameBuf)
	p.Controller.Version = fmt.Sprintf("%d.%d", p.Controller.Major, p.Controller.Minor)

	if p.config.OnAttribute != nil {
		p.config.OnAttribute()
	}
}

func NewOriginator(addr string, slot uint8, cfg *Config) (*plc, error) {
	_plc := &plc{}
	_plc.slot = slot
	_plc.config = cfg
	if _plc.config == nil {
		_plc.config = defaultConfig
	}

	_tcp, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", addr, _plc.config.ENIP_PORT))
	if err != nil {
		return nil, err
	}

	_plc.tcpAddr = _tcp
	_plc.request = &encapsulation.Request{}

	_plc.path = segment.PortBuild(1, []byte{slot})

	rand.Seed(time.Now().Unix())
	_plc.context = rand.Uint64()
	_plc.config.Printf("Random context: %d\n", _plc.context)
	_plc.Controller = &controller{}

	_plc.sender = make(chan []byte)
	return _plc, nil
}
