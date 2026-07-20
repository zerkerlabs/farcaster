// Package awskms adapts a live AWS KMS client to the facilitator's
// chain.KMSAPI (spec 0007 Decision 2). It is the only place the AWS SDK is
// imported: the gas-key custody logic (secp256k1 DER handling, low-S, recovery
// id) lives in package chain and is tested against a fake, so this adapter is a
// thin, dependency-isolating shim that pins the key to ECC_SECG_P256K1 signing
// with a pre-computed digest.
//
// Provisioning the KMS key, its ARN, and the IAM policy is managed ops
// configuration (out of scope for T3); this package only consumes an already
// provisioned key by id.
package awskms

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Client wraps an AWS KMS client scoped to one key, satisfying chain.KMSAPI.
type Client struct {
	kms   *kms.Client
	keyID string
}

// New returns an adapter over the given KMS client for the given key id (a key
// id, ARN, or alias). The key must be an asymmetric ECC_SECG_P256K1 signing
// key; that is enforced by AWS at sign time, not here.
func New(client *kms.Client, keyID string) *Client {
	return &Client{kms: client, keyID: keyID}
}

// GetPublicKey returns the DER-encoded SubjectPublicKeyInfo for the key.
func (c *Client) GetPublicKey(ctx context.Context) ([]byte, error) {
	out, err := c.kms.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: &c.keyID})
	if err != nil {
		return nil, fmt.Errorf("awskms: get public key: %w", err)
	}
	return out.PublicKey, nil
}

// Sign signs the 32-byte digest with the key using ECDSA_SHA_256 over a
// pre-computed digest (MessageType DIGEST), returning the DER ECDSA signature.
func (c *Client) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	out, err := c.kms.Sign(ctx, &kms.SignInput{
		KeyId:            &c.keyID,
		Message:          digest,
		MessageType:      kmstypes.MessageTypeDigest,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("awskms: sign: %w", err)
	}
	return out.Signature, nil
}
