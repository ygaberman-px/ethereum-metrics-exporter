package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
)

// Container is the state container.
type Container struct {
	log     logrus.FieldLogger
	spec    *Spec
	genesis *v1.Genesis
	epochs  Epochs

	currentEpoch phase0.Epoch
	currentSlot  phase0.Slot

	callbacksEpochChanged     []func(ctx context.Context, epoch phase0.Epoch) error
	callbacksSlotChanged      []func(ctx context.Context, slot phase0.Slot) error
	callbacksEpochSlotChanged []func(ctx context.Context, epoch phase0.Epoch, slot phase0.Slot) error
	callbacksBlockInserted    []func(ctx context.Context, epoch phase0.Epoch, slot Slot) error
}

const (
	// SurroundingEpochDistance is the number of epochs to create around the current epoch.
	SurroundingEpochDistance = 1
)

// NewContainer creates a new state container instance
func NewContainer(ctx context.Context, log logrus.FieldLogger, sp *Spec, genesis *v1.Genesis) Container {
	return Container{
		log:  log,
		spec: sp,

		genesis: genesis,

		currentEpoch: 0,
		currentSlot:  0,

		epochs: NewEpochs(sp, genesis),
	}
}

var (
	ErrSpecNotInitialized = errors.New("spec not initialized")
	ErrGenesisNotFetched  = errors.New("genesis not fetched")
)

// Init initializes the state container.
func (c *Container) Init(ctx context.Context) error {
	if err := c.hydrateEpochs(ctx); err != nil {
		return err
	}

	go c.ticker(ctx)

	//nolint:errcheck // dont care about an error here.
	go c.currentSlotLoop(ctx)

	return nil
}

// Spec returns the spec for the state container.
func (c *Container) Spec() *Spec {
	return c.spec
}

func (c *Container) ticker(ctx context.Context) {
	c.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 5):
			c.tick(ctx)
		}
	}
}

func (c *Container) currentSlotLoop(ctx context.Context) error {
	for {
		currentSlot := c.currentSlot

		nextSlotStartsAt := c.genesis.GenesisTime.Add(c.spec.SecondsPerSlot * time.Duration(currentSlot+1))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(nextSlotStartsAt)):
			if err := c.checkForNewCurrentEpochAndSlot(ctx); err != nil {
				return err
			}
		}
	}
}

func (c *Container) tick(ctx context.Context) {
	if err := c.hydrateEpochs(ctx); err != nil {
		c.log.WithError(err).Error("Failed to hydrate epochs")
	}

	if err := c.checkForNewCurrentEpochAndSlot(ctx); err != nil {
		c.log.WithError(err).Error("Failed to check for new current epoch and slot")
	}
}

// AddBeaconBlock adds a beacon block to the state container.
func (c *Container) AddBeaconBlock(ctx context.Context, beaconBlock *spec.VersionedSignedBeaconBlock, seenAt time.Time) error {
	if beaconBlock == nil {
		return errors.New("beacon block is nil")
	}

	// Calculate the epoch
	slotNumber, err := beaconBlock.Slot()
	if err != nil {
		return err
	}

	epochNumber := c.calculateEpochFromSlot(slotNumber)

	if exists := c.epochs.Exists(epochNumber); !exists {
		if _, err = c.createEpoch(ctx, epochNumber); err != nil {
			return err
		}
	}

	// Get the epoch
	epoch, err := c.epochs.GetEpoch(epochNumber)
	if err != nil {
		return err
	}

	// Insert the block
	//nolint:gocritic // false positive
	if err = epoch.AddBlock(beaconBlock, seenAt); err != nil {
		return err
	}

	slot, err := epoch.GetSlot(slotNumber)
	if err != nil {
		return err
	}

	delay, err := slot.ProposerDelay()
	if err != nil {
		return err
	}

	proposer := "unknown"

	proposerDuty, err := slot.ProposerDuty()
	if err == nil {
		proposer = fmt.Sprintf("%v", proposerDuty.ValidatorIndex)
	} else {
		c.log.WithError(err).WithField("slot", slot).Warn("Failed to get slot proposer")
	}

	c.log.WithFields(logrus.Fields{
		"epoch":          epochNumber,
		"slot":           slotNumber,
		"proposer_delay": delay.String(),
		"proposer_index": proposer,
	}).Info("Inserted beacon block")

	c.publishBlockInserted(ctx, epochNumber, *slot)

	return nil
}

func (c *Container) hydrateEpochs(ctx context.Context) error {
	epoch := c.currentEpoch

	// Ensure the state has +-SurroundingEpochDistance epochs created.
	for i := epoch - SurroundingEpochDistance; i <= epoch+SurroundingEpochDistance; i++ {
		if _, err := c.epochs.GetEpoch(i); err != nil {
			if _, err := c.createEpoch(ctx, i); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Container) getCurrentEpochAndSlot() (phase0.Epoch, phase0.Slot, error) {
	if c.spec == nil {
		return 0, 0, ErrSpecNotInitialized
	}

	if c.genesis == nil {
		return 0, 0, ErrGenesisNotFetched
	}

	if err := c.spec.Validate(); err != nil {
		return 0, 0, err
	}

	// Calculate the current epoch based on genesis time.
	genesis := c.genesis.GenesisTime

	currentSlot := phase0.Slot(time.Since(genesis).Seconds() / c.spec.SecondsPerSlot.Seconds())
	currentEpoch := phase0.Epoch(currentSlot / c.spec.SlotsPerEpoch)

	return currentEpoch, currentSlot, nil
}

func (c *Container) SetProposerDuties(ctx context.Context, epochNumber phase0.Epoch, duties []*v1.ProposerDuty) error {
	epoch, err := c.epochs.GetEpoch(epochNumber)
	if err != nil {
		return err
	}

	if err := epoch.SetProposerDuties(duties); err != nil {
		return err
	}

	return nil
}

func (c *Container) createEpoch(ctx context.Context, epochNumber phase0.Epoch) (*Epoch, error) {
	epoch, err := c.epochs.NewInitializedEpoch(epochNumber)
	if err != nil {
		return nil, err
	}

	return epoch, nil
}

func (c *Container) checkForNewCurrentEpochAndSlot(ctx context.Context) error {
	epoch, slot, err := c.getCurrentEpochAndSlot()
	if err != nil {
		return err
	}

	epochChanged := false

	if epoch != c.currentEpoch {
		c.currentEpoch = epoch

		epochChanged = true

		// Notify the listeners of the new epoch.
		go c.publishEpochChanged(ctx, epoch)
	}

	slotChanged := false

	if slot != c.currentSlot {
		c.currentSlot = slot

		slotChanged = true

		// Notify the listeners of the new slot.
		go c.publishSlotChanged(ctx, slot)
	}

	if epochChanged || slotChanged {
		// Notify the listeners of the new epoch and slot.
		go c.publishEpochSlotChanged(ctx, epoch, slot)
	}

	return nil
}

// GetSlot returns the slot for the given slot number.
func (c *Container) GetSlot(ctx context.Context, slotNumber phase0.Slot) (*Slot, error) {
	epoch, err := c.epochs.GetEpoch(c.calculateEpochFromSlot(slotNumber))
	if err != nil {
		return nil, err
	}

	return epoch.GetSlot(slotNumber)
}

func (c *Container) calculateEpochFromSlot(slotNumber phase0.Slot) phase0.Epoch {
	return phase0.Epoch(slotNumber / c.spec.SlotsPerEpoch)
}

// GetEpoch returns the epoch for the given epoch number.
func (c *Container) GetEpoch(ctx context.Context, epochNumber phase0.Epoch) (*Epoch, error) {
	return c.epochs.GetEpoch(epochNumber)
}

func (c *Container) DeleteEpoch(ctx context.Context, epochNumber phase0.Epoch) error {
	return c.epochs.RemoveEpoch(epochNumber)
}
