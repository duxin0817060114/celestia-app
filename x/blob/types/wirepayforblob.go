package types

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/celestiaorg/celestia-app/pkg/appconsts"
	shares "github.com/celestiaorg/celestia-app/pkg/shares"
	"github.com/celestiaorg/nmt/namespace"
	sdkclient "github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"golang.org/x/exp/constraints"
)

var _ sdk.Msg = &MsgWirePayForBlob{}

// NewWirePayForBlob creates a new MsgWirePayForBlob by using the namespace and
// blob to generate a share commitment. Note that the generated share
// commitment still needs to be signed using the SignShareCommitment method.
func NewWirePayForBlob(namespace, blob []byte) (*MsgWirePayForBlob, error) {
	// sanity check namespace ID size
	if len(namespace) != NamespaceIDSize {
		return nil, ErrInvalidNamespaceLen.Wrapf("got: %d want: %d",
			len(namespace),
			NamespaceIDSize,
		)
	}

	out := &MsgWirePayForBlob{
		NamespaceId:     namespace,
		BlobSize:        uint64(len(blob)),
		Blob:            blob,
		ShareCommitment: &ShareCommitAndSignature{},
	}

	// generate the share commitment
	commit, err := CreateCommitment(namespace, blob)
	if err != nil {
		return nil, err
	}
	out.ShareCommitment = &ShareCommitAndSignature{ShareCommitment: commit}
	return out, nil
}

// SignShareCommitment creates and signs the share commitment associated
// with a MsgWirePayForBlob.
func (msg *MsgWirePayForBlob) SignShareCommitment(signer *KeyringSigner, options ...TxBuilderOption) error {
	addr, err := signer.GetSignerInfo().GetAddress()
	if err != nil {
		return err
	}

	if addr == nil {
		return errors.New("failed to get address")
	}
	if addr.Empty() {
		return errors.New("failed to get address")
	}

	msg.Signer = addr.String()
	// create an entire MsgPayForBlob and sign over it (including the signature in the commitment)
	builder := signer.NewTxBuilder(options...)

	sig, err := msg.createPayForBlobSignature(signer, builder)
	if err != nil {
		return err
	}
	msg.ShareCommitment.Signature = sig
	return nil
}

func (msg *MsgWirePayForBlob) Route() string { return RouterKey }

// ValidateBasic checks for valid namespace length, declared blob size, share
// commitments, signatures for those share commitment, and fulfills the sdk.Msg
// interface.
func (msg *MsgWirePayForBlob) ValidateBasic() error {
	if err := ValidateMessageNamespaceID(msg.GetNamespaceId()); err != nil {
		return err
	}

	if _, err := sdk.AccAddressFromBech32(msg.Signer); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("invalid 'from' address: %s", err)
	}

	// make sure that the blob size matches the actual size of the blob
	if msg.BlobSize != uint64(len(msg.Blob)) {
		return ErrDeclaredActualDataSizeMismatch.Wrapf(
			"declared: %d vs actual: %d",
			msg.BlobSize,
			len(msg.Blob),
		)
	}

	return msg.ValidateMessageShareCommitment()
}

// ValidateMessageShareCommitment returns an error if the share
// commitment is invalid.
func (msg *MsgWirePayForBlob) ValidateMessageShareCommitment() error {
	// check that the commit is valid
	commit := msg.ShareCommitment
	calculatedCommit, err := CreateCommitment(msg.GetNamespaceId(), msg.Blob)
	if err != nil {
		return ErrCalculateCommit.Wrap(err.Error())
	}

	if !bytes.Equal(calculatedCommit, commit.ShareCommitment) {
		return ErrInvalidShareCommit
	}

	return nil
}

// ValidateMessageNamespaceID returns an error if the provided namespace.ID is an invalid or reserved namespace id.
func ValidateMessageNamespaceID(ns namespace.ID) error {
	// ensure that the namespace id is of length == NamespaceIDSize
	if nsLen := len(ns); nsLen != NamespaceIDSize {
		return ErrInvalidNamespaceLen.Wrapf("got: %d want: %d",
			nsLen,
			NamespaceIDSize,
		)
	}
	// ensure that a reserved namespace is not used
	if bytes.Compare(ns, appconsts.MaxReservedNamespace) < 1 {
		return ErrReservedNamespace.Wrapf("got namespace: %x, want: > %x", ns, appconsts.MaxReservedNamespace)
	}

	// ensure that ParitySharesNamespaceID is not used
	if bytes.Equal(ns, appconsts.ParitySharesNamespaceID) {
		return ErrParitySharesNamespace
	}

	// ensure that TailPaddingNamespaceID is not used
	if bytes.Equal(ns, appconsts.TailPaddingNamespaceID) {
		return ErrTailPaddingNamespace
	}

	return nil
}

// GetSigners returns the addresses of the message signers
func (msg *MsgWirePayForBlob) GetSigners() []sdk.AccAddress {
	address, err := sdk.AccAddressFromBech32(msg.Signer)
	if err != nil {
		panic(err)
	}
	return []sdk.AccAddress{address}
}

// createPayForBlobSignature generates the signature for a MsgPayForBlob for a
// single squareSize using the info from a MsgWirePayForBlob.
func (msg *MsgWirePayForBlob) createPayForBlobSignature(signer *KeyringSigner, builder sdkclient.TxBuilder) ([]byte, error) {
	pfb, err := msg.unsignedPayForBlob()
	if err != nil {
		return nil, err
	}
	tx, err := signer.BuildSignedTx(builder, pfb)
	if err != nil {
		return nil, err
	}
	sigs, err := tx.GetSignaturesV2()
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 {
		return nil, fmt.Errorf("expected a single signer: got %d", len(sigs))
	}
	sig, ok := sigs[0].Data.(*signing.SingleSignatureData)
	if !ok {
		return nil, fmt.Errorf("expected a single signer")
	}
	return sig.Signature, nil
}

// unsignedPayForBlob uses the data in the MsgWirePayForBlob
// to create a new MsgPayForBlob.
func (msg *MsgWirePayForBlob) unsignedPayForBlob() (*MsgPayForBlob, error) {
	// create the commitment using the blob
	commit, err := CreateCommitment(msg.NamespaceId, msg.Blob)
	if err != nil {
		return nil, err
	}

	spfb := MsgPayForBlob{
		NamespaceId:     msg.NamespaceId,
		BlobSize:        msg.BlobSize,
		ShareCommitment: commit,
		Signer:          msg.Signer,
	}
	return &spfb, nil
}

// ProcessWirePayForBlob performs the malleation process that occurs before
// creating a block. It parses the MsgWirePayForBlob to produce the components
// needed to create a single MsgPayForBlob.
func ProcessWirePayForBlob(msg *MsgWirePayForBlob) (*tmproto.Blob, *MsgPayForBlob, []byte, error) {
	// add the blob to the list of core blobs to be returned to celestia-core
	coreMsg := tmproto.Blob{
		NamespaceId: msg.GetNamespaceId(),
		Data:        msg.GetBlob(),
	}

	// wrap the signed transaction data
	pfb, err := msg.unsignedPayForBlob()
	if err != nil {
		return nil, nil, nil, err
	}

	return &coreMsg, pfb, msg.ShareCommitment.Signature, nil
}

// HasWirePayForBlob performs a quick but not definitive check to see if a tx
// contains a MsgWirePayForBlob. The check is quick but not definitive because
// it only uses a proto.Message generated method instead of performing a full
// type check.
func HasWirePayForBlob(tx sdk.Tx) bool {
	for _, msg := range tx.GetMsgs() {
		msgName := sdk.MsgTypeURL(msg)
		if msgName == URLMsgWirePayForBlob {
			return true
		}
	}
	return false
}

// ExtractMsgWirePayForBlob attempts to extract a MsgWirePayForBlob from a
// provided sdk.Tx. It returns an error if no MsgWirePayForBlob is found.
func ExtractMsgWirePayForBlob(tx sdk.Tx) (*MsgWirePayForBlob, error) {
	noWirePFBError := errors.New("sdk.Tx does not contain MsgWirePayForBlob sdk.Msg")
	// perform a quick check before attempting a type check
	if !HasWirePayForBlob(tx) {
		return nil, noWirePFBError
	}

	// only support malleated transactions that contain a single sdk.Msg
	if len(tx.GetMsgs()) != 1 {
		return nil, errors.New("sdk.Txs with a single MsgWirePayForBlob are currently supported")
	}

	msg := tx.GetMsgs()[0]
	wireMsg, ok := msg.(*MsgWirePayForBlob)
	if !ok {
		return nil, noWirePFBError
	}

	return wireMsg, nil
}

// MsgMinSquareSize returns the minimum square size that msgSize can be included
// in. The returned square size does not account for the associated transaction
// shares or non-interactive defaults so it is a minimum.
func MsgMinSquareSize[T constraints.Integer](msgSize T) T {
	shareCount := shares.MsgSharesUsed(int(msgSize))
	return T(MinSquareSize(shareCount))
}

// MinSquareSize returns the minimum square size that can contain shareCount
// number of shares.
func MinSquareSize[T constraints.Integer](shareCount T) T {
	return T(shares.RoundUpPowerOfTwo(uint64(math.Ceil(math.Sqrt(float64(shareCount))))))
}
