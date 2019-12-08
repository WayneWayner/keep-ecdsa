package tss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/binance-chain/tss-lib/tss"
	"github.com/keep-network/keep-tecdsa/pkg/net"
)

// NetworkBridge is used to communicate with network provider on broadcast and
// unicast channels. It broadcast and unicast channels.
type NetworkBridge struct {
	networkProvider net.Provider

	groupMembersIDs []MemberID

	party   tss.Party
	params  *tss.Parameters
	errChan chan<- error

	channelsMutex     *sync.Mutex
	broadcastChannels map[string]net.BroadcastChannel
	unicastChannels   map[string]net.UnicastChannel
}

// NewNetworkBridge initializes a new network bridge for given network provider.
func NewNetworkBridge(networkProvider net.Provider) *NetworkBridge {
	return &NetworkBridge{
		networkProvider:   networkProvider,
		channelsMutex:     &sync.Mutex{},
		broadcastChannels: make(map[string]net.BroadcastChannel),
		unicastChannels:   make(map[string]net.UnicastChannel),
	}
}

func (b *NetworkBridge) start(
	groupMembersIDs []MemberID,
	party tss.Party,
	params *tss.Parameters,
	outChan <-chan tss.Message,
	errChan chan<- error,
) error {
	b.groupMembersIDs = groupMembersIDs
	b.party = party
	b.params = params
	b.errChan = errChan

	recvMessage := make(chan *TSSMessage, params.PartyCount())

	if err := b.initializeChannels(recvMessage); err != nil {
		return fmt.Errorf("failed to initialize channels: [%v]", err)
	}

	go func() {
		for {
			select {
			case tssLibMsg := <-outChan:
				go b.sendMessage(tssLibMsg)
			case msg := <-recvMessage:
				go b.receiveMessage(msg)
			}
		}
	}()

	return nil
}

func (b *NetworkBridge) initializeChannels(recvMessageChan chan *TSSMessage) error {
	handleMessageFunc := func(channel chan<- *TSSMessage) net.HandleMessageFunc {
		return net.HandleMessageFunc{
			Type: TSSmessageType,
			Handler: func(msg net.Message) error {
				switch tssMessage := msg.Payload().(type) {
				case *TSSMessage:
					channel <- tssMessage
				default:
					return fmt.Errorf("unexpected message: [%v]", msg.Payload())
				}

				return nil
			},
		}
	}

	// Initialize broadcast channel.
	broadcastChannel, err := b.broadcastChannelFor(broadcastChannelName(b.groupMembersIDs))
	if err != nil {
		return fmt.Errorf("failed to get broadcast channel: [%v]", err)
	}

	if err := broadcastChannel.Recv(handleMessageFunc(recvMessageChan)); err != nil {
		return fmt.Errorf("failed to register receive handler for broadcast channel: [%v]", err)
	}

	// Initialize unicast channels.
	for _, peerPartyID := range b.params.Parties().IDs() {
		if bytes.Equal(peerPartyID.GetKey(), b.party.PartyID().GetKey()) {
			continue
		}

		unicastChannel, err := b.unicastChannelWith(unicastChannelName(*peerPartyID))
		if err != nil {
			return fmt.Errorf("failed to get unicast channel: [%v]", err)
		}

		if err := unicastChannel.Recv(handleMessageFunc(recvMessageChan)); err != nil {
			return fmt.Errorf("failed to register receive handler for unicast channel: [%v]", err)
		}
	}

	return nil
}

func (b *NetworkBridge) broadcastChannelFor(name string) (net.BroadcastChannel, error) {
	b.channelsMutex.Lock()
	defer b.channelsMutex.Unlock()

	broadcastChannel, exists := b.broadcastChannels[name]
	if exists {
		return broadcastChannel, nil
	}

	broadcastChannel, err := b.networkProvider.BroadcastChannelFor(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get broadcast channel: [%v]", err)
	}

	if err := broadcastChannel.RegisterUnmarshaler(func() net.TaggedUnmarshaler {
		return &TSSMessage{}
	}); err != nil {
		return nil, fmt.Errorf("failed to register unmarshaler for broadcast channel: [%v]", err)
	}

	b.broadcastChannels[name] = broadcastChannel

	return broadcastChannel, nil
}

func broadcastChannelName(members []MemberID) string {
	ids := []string{}
	for _, id := range members {
		ids = append(ids, string(id))
	}

	digest := sha256.Sum256([]byte(strings.Join(ids, "-")))

	return hex.EncodeToString(digest[:])
}

func (b *NetworkBridge) unicastChannelWith(peer string) (net.UnicastChannel, error) {
	b.channelsMutex.Lock()
	defer b.channelsMutex.Unlock()

	unicastChannel, exists := b.unicastChannels[peer]
	if exists {
		return unicastChannel, nil
	}

	unicastChannel, err := b.networkProvider.UnicastChannelWith(peer)
	if err != nil {
		return nil, fmt.Errorf("failed to get unicast channel: [%v]", err)
	}

	if err := unicastChannel.RegisterUnmarshaler(func() net.TaggedUnmarshaler {
		return &TSSMessage{}
	}); err != nil {
		return nil, fmt.Errorf("failed to register unmarshaler for unicast channel: [%v]", err)
	}

	b.unicastChannels[peer] = unicastChannel

	return unicastChannel, nil
}

func unicastChannelName(peerPartyID tss.PartyID) string {
	return peerPartyID.KeyInt().String()
}

func (b *NetworkBridge) sendMessage(tssLibMsg tss.Message) {
	bytes, routing, err := tssLibMsg.WireBytes()
	if err != nil {
		b.errChan <- fmt.Errorf("failed to encode message: [%v]", b.party.WrapError(err))
		return
	}

	msg := &TSSMessage{
		SenderID:    routing.From.GetKey(),
		Payload:     bytes,
		IsBroadcast: routing.IsBroadcast,
	}

	if routing.To == nil {
		channelName := broadcastChannelName(b.groupMembersIDs)
		broadcastChannel, err := b.broadcastChannelFor(channelName)
		if err != nil {
			b.errChan <- fmt.Errorf("failed to find unicast channel: [%v]", channelName)
			return
		}

		if broadcastChannel.Send(msg); err != nil {
			b.errChan <- fmt.Errorf("failed to send broadcast message: [%v]", err)
			return
		}
	} else {
		for _, destination := range routing.To {
			channelName := unicastChannelName(*destination)

			unicastChannel, err := b.unicastChannelWith(channelName)
			if err != nil {
				b.errChan <- fmt.Errorf("failed to find unicast channel: [%v]", channelName)
				continue
			}

			if err := unicastChannel.Send(msg); err != nil {
				b.errChan <- fmt.Errorf("failed to send unicast message: [%v]", err)
				continue
			}
		}
	}
}

func (b *NetworkBridge) receiveMessage(msg *TSSMessage) {
	senderKey := new(big.Int).SetBytes(msg.SenderID)
	senderPartyID := b.params.Parties().IDs().FindByKey(senderKey)

	if senderPartyID == b.party.PartyID() {
		return
	}

	bytes := msg.Payload

	_, err := b.party.UpdateFromBytes(
		bytes,
		senderPartyID,
		msg.IsBroadcast,
	)
	if err != nil {
		b.errChan <- fmt.Errorf("failed to update party: [%v]", b.party.WrapError(err))
	}
}

func (b *NetworkBridge) close() error {
	if err := b.unregisterRecvs(); err != nil {
		return fmt.Errorf("failed to unregister receivers: [%v]", err)
	}

	return nil
}

func (b *NetworkBridge) unregisterRecvs() error {
	for _, broadcastChannel := range b.broadcastChannels {
		if err := broadcastChannel.UnregisterRecv(TSSmessageType); err != nil {
			return fmt.Errorf(
				"failed to unregister receive handler for broadcast channel: [%v]",
				err,
			)
		}

	}
	for _, unicastChannel := range b.unicastChannels {
		if err := unicastChannel.UnregisterRecv(TSSmessageType); err != nil {
			return fmt.Errorf(
				"failed to unregister receive handler for unicast channel: [%v]",
				err,
			)
		}
	}

	return nil
}