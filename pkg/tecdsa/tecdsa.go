// Package tecdsa defines Keep tECDSA client.
package tecdsa

import (
	crand "crypto/rand"
	"fmt"
	"github.com/ipfs/go-log"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/keep-network/keep-tecdsa/pkg/chain/eth"
	"github.com/keep-network/keep-tecdsa/pkg/ecdsa"
)

var logger = log.Logger("keep-tecdsa")

// client holds blockchain specific configuration, interfaces to interact with the
// blockchain and a map of signers for given keeps.
type client struct {
	ethereumChain    eth.Interface
	bitcoinNetParams *chaincfg.Params
	keepsSigners     sync.Map //<keepAddress, signer>
}

// Initialize initializes the tECDSA client with rules related to events handling.
func Initialize(
	ethereumChain eth.Interface,
	bitcoinNetParams *chaincfg.Params,
) error {
	client := &client{
		ethereumChain:    ethereumChain,
		bitcoinNetParams: bitcoinNetParams,
	}

	ethereumChain.OnECDSAKeepCreated(func(event *eth.ECDSAKeepCreatedEvent) {
		logger.Infof(
			"new keep created with address: [%s]",
			event.KeepAddress.String(),
		)

		go func() {
			if err := client.generateSignerForKeep(event.KeepAddress); err != nil {
				logger.Errorf("signer generation failed: [%v]", err)
			}
		}()

		ethereumChain.OnSignatureRequested(
			event.KeepAddress,
			func(signatureRequestedEvent *eth.SignatureRequestedEvent) {
				logger.Debugf(
					"new signature requested for digest: [%+x]",
					signatureRequestedEvent.Digest,
				)

				go func() {
					err := client.calculateSignatureForKeep(
						event.KeepAddress,
						signatureRequestedEvent.Digest,
					)

					if err != nil {
						logger.Errorf("signature calculation failed: [%v]", err)
					}
				}()
			},
		)
	})

	return nil
}

// generateSignerForKeep generates a new signer with ECDSA key pair and calculates
// bitcoin specific P2WPKH address based on signer's public key. It stores the
// signer in a map assigned to a provided keep address.
func (c *client) generateSignerForKeep(keepAddress eth.KeepAddress) error {
	signer, err := generateSigner()

	logger.Debugf(
		"generated signer with public key: [x: [%x], y: [%x]]",
		signer.PublicKey().X,
		signer.PublicKey().Y,
	)

	// Publish signer's public key on ethereum blockchain in a specific keep
	// contract.
	serializedPublicKey, err := eth.SerializePublicKey(signer.PublicKey())
	if err != nil {
		return fmt.Errorf("failed to serialize public key: [%v]", err)
	}

	err = c.ethereumChain.SubmitKeepPublicKey(
		keepAddress,
		serializedPublicKey,
	)
	if err != nil {
		return fmt.Errorf("failed to submit public key: [%v]", err)
	}

	logger.Infof(
		"initialized signer for keep [%s]",
		keepAddress.String(),
	)

	// Store the signer in a map, with the keep address as a key. Keep address
	// is converted to a hexadecimal string prefiexed with `0x`.
	c.keepsSigners.Store(keepAddress.String(), signer)

	return nil
}

func generateSigner() (*ecdsa.Signer, error) {
	privateKey, err := ecdsa.GenerateKey(crand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: [%v]", err)
	}

	return ecdsa.NewSigner(privateKey), nil
}

func (c *client) calculateSignatureForKeep(keepAddress eth.KeepAddress, digest [32]byte) error {
	signer, ok := c.keepsSigners.Load(keepAddress.String())
	if !ok {
		return fmt.Errorf("failed to find signer for keep: [%s]", keepAddress.String())
	}

	signature, err := signer.(*ecdsa.Signer).CalculateSignature(
		crand.Reader,
		digest[:],
	)

	logger.Debugf(
		"signature calculated:\nr: [%#x]\ns: [%#x]\nrecovery ID: [%d]\n",
		signature.R,
		signature.S,
		signature.RecoveryID,
	)

	err = c.ethereumChain.SubmitSignature(
		keepAddress,
		digest,
		signature,
	)
	if err != nil {
		return fmt.Errorf("failed to submit signature: [%v]", err)
	}

	return nil
}