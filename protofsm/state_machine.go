package protofsm

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// pollInterval is the interval at which we'll poll the SendWhen
	// predicate if specified.
	pollInterval = time.Millisecond * 100
)

// EmittedEvent is a special type that can be emitted by a state transition.
// This can container internal events which are to be routed back to the state,
// or external events which are to be sent to the daemon.
type EmittedEvent[Event any] struct {
	// InternalEvent is an optional internal event that is to be routed
	// back to the target state. This enables state to trigger one or many
	// state transitions without a new external event.
	InternalEvent fn.Option[Event]

	// ExternalEvent is an optional external event that is to be sent to
	// the daemon for dispatch. Usually, this is some form of I/O.
	ExternalEvents fn.Option[DaemonEventSet]
}

// StateTransition is a state transition type. It denotes the next state to go
// to, and also the set of events to emit.
type StateTransition[Event any, Env Environment] struct {
	// NextState is the next state to transition to.
	NextState State[Event, Env]

	// NewEvents is the set of events to emit.
	NewEvents fn.Option[EmittedEvent[Event]]
}

// Environment is an abstract interface that represents the environment that
// the state machine will execute using. From the PoV of the main state machine
// executor, we just care about being able to clean up any resources that were
// allocated by the environment.
type Environment interface {
	// CleanUp is a method that'll be called once the state machine has
	// reached a terminal state. It signals the end of the execution of the
	// state machine.
	CleanUp() error

	// Name returns the name of the environment. This is used to uniquely
	// identify the environment of related state machines.
	Name() string

	// TODO(roasbeef): also add checkpointing?
}

// State defines an abstract state along, namely its state transition function
// that takes as input an event and an environment, and returns a state
// transition (next state, and set of events to emit). As state can also either
// be terminal, or not, a terminal event causes state execution to halt.
type State[Event any, Env Environment] interface {
	// ProcessEvent takes an event and an environment, and returns a new
	// state transition. This will be iteratively called until either a
	// terminal state is reached, or no further internal events are
	// emitted.
	ProcessEvent(event Event, env Env) (*StateTransition[Event, Env], error)

	// IsTerminal returns true if this state is terminal, and false
	// otherwise.
	IsTerminal() bool

	// TODO(roasbeef): also add state serialization?
}

// DaemonAdapters is a set of methods that server as adapters to bridge the
// pure world of the FSM to the real world of the daemon. These will be used to
// do things like broadcast transactions, or send messages to peers.
type DaemonAdapters interface {
	// SendMessages sends the target set of messages to the target peer.
	SendMessages(btcec.PublicKey, []lnwire.Message) error

	// BroadcastTransaction broadcasts a transaction with the target label.
	BroadcastTransaction(*wire.MsgTx, string) error

	// DisableChannel disables the target channel.
	DisableChannel(wire.OutPoint) error

	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. We also pass in the pkScript as
	// the default light client instead needs to match on scripts created
	// in the block. If a nil txid is passed in, then not only should we
	// match on the script, but we should also dispatch once the
	// transaction containing the script reaches numConfs confirmations.
	// This can be useful in instances where we only know the script in
	// advance, but not the transaction containing it.
	//
	// TODO(roasbeef): could abstract further?
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32,
		opts ...chainntnfs.NotifierOption,
	) (*chainntnfs.ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The script that
	// the outpoint creates must also be specified. This allows this
	// interface to be implemented by BIP 158-like filtering.
	RegisterSpendNtfn(outpoint *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*chainntnfs.SpendEvent, error)
}

// stateQuery is used by outside callers to query the internal state of the
// state machine.
type stateQuery[Event any, Env Environment] struct {
	// CurrentState is a channel that will be sent the current state of the
	// state machine.
	CurrentState chan State[Event, Env]
}

// StateMachine represents an abstract FSM that is able to process new incoming
// events and drive a state machine to termination. This implementation uses
// type params to abstract over the types of events and environment. Events
// trigger new state transitions, that use the environment to perform some
// action.
//
// TODO(roasbeef): terminal check, daemon event execution, init?
type StateMachine[Event any, Env Environment] struct {
	cfg StateMachineCfg[Event, Env]

	// events is the channel that will be used to send new events to the
	// FSM.
	events chan Event

	// newStateEvents is an EventDistributor that will be used to notify
	// any relevant callers of new state transitions that occur.
	newStateEvents *fn.EventDistributor[State[Event, Env]]

	// stateQuery is a channel that will be used by outside callers to
	// query the internal state machine state.
	stateQuery chan stateQuery[Event, Env]

	startOnce sync.Once
	stopOnce  sync.Once

	// TODO(roasbeef): also use that context guard here?
	quit chan struct{}
	wg   sync.WaitGroup
}

// ErrorReporter is an interface that's used to report errors that occur during
// state machine execution.
type ErrorReporter interface {
	// ReportError is a method that's used to report an error that occurred
	// during state machine execution.
	ReportError(err error)
}

// StateMachineCfg is a configuration struct that's used to create a new state
// machine.
type StateMachineCfg[Event any, Env Environment] struct {
	// ErrorReporter is used to report errors that occur during state
	// transitions.
	ErrorReporter ErrorReporter

	// Daemon is a set of adapters that will be used to bridge the FSM to
	// the daemon.
	Daemon DaemonAdapters

	// InitialState is the initial state of the state machine.
	InitialState State[Event, Env]

	// Env is the environment that the state machine will use to execute.
	Env Env

	// InitEvent is an optional event that will be sent to the state
	// machine as if it was emitted at the onset of the state machine. This
	// can be used to set up tracking state such as a txid confirmation
	// event.
	InitEvent fn.Option[DaemonEvent]

	// MsgMapper is an optional message mapper that can be used to map
	// normal wire messages into FSM events.
	MsgMapper fn.Option[MsgMapper[Event]]
}

// NewStateMachine creates a new state machine given a set of daemon adapters,
// an initial state, an environment, and an event to process as if emitted at
// the onset of the state machine. Such an event can be used to set up tracking
// state such as a txid confirmation event.
func NewStateMachine[Event any, Env Environment](cfg StateMachineCfg[Event, Env], //nolint:lll
) StateMachine[Event, Env] {

	return StateMachine[Event, Env]{
		cfg:            cfg,
		events:         make(chan Event, 1),
		stateQuery:     make(chan stateQuery[Event, Env]),
		quit:           make(chan struct{}),
		newStateEvents: fn.NewEventDistributor[State[Event, Env]](),
	}
}

// Start starts the state machine. This will spawn a goroutine that will drive
// the state machine to completion.
func (s *StateMachine[Event, Env]) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.driveMachine()
	})
}

// Stop stops the state machine. This will block until the state machine has
// reached a stopping point.
func (s *StateMachine[Event, Env]) Stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		s.wg.Wait()
	})
}

// SendEvent sends a new event to the state machine.
//
// TODO(roasbeef): bool if processed?
func (s *StateMachine[Event, Env]) SendEvent(event Event) {
	log.Debugf("FSM(%v): sending event: %v", s.cfg.Env.Name(),
		newLogClosure(func() string {
			return spew.Sdump(event)
		}),
	)

	select {
	case s.events <- event:
	case <-s.quit:
		return
	}
}

// CanHandle returns true if the target message can be routed to the state
// machine.
func (s *StateMachine[Event, Env]) CanHandle(msg lnwire.Message) bool {
	cfgMapper := s.cfg.MsgMapper
	return fn.MapOptionZ(cfgMapper, func(mapper MsgMapper[Event]) bool {
		return mapper.MapMsg(msg).IsSome()
	})
}

// Name returns the name of the state machine's environment.
func (s *StateMachine[Event, Env]) Name() string {
	return s.cfg.Env.Name()
}

// SendMessage attempts to send a wire message to the state machine. If the
// message can be mapped using the default message mapper, then true is
// returned indicating that the message was processed. Otherwise, false is
// returned.
func (s *StateMachine[Event, Env]) SendMessage(msg lnwire.Message) bool {
	// If we have no message mapper, then return false as we can't process
	// this message.
	if !s.cfg.MsgMapper.IsSome() {
		return false
	}

	log.Debugf("FSM(%v): sending msg: %v", s.cfg.Env.Name(),
		newLogClosure(func() string {
			return spew.Sdump(msg)
		}),
	)

	// Otherwise, try to map the message using the default message mapper.
	// If we can't extract an event, then we'll return false to indicate
	// that the message wasn't processed.
	var processed bool
	s.cfg.MsgMapper.WhenSome(func(mapper MsgMapper[Event]) {
		event := mapper.MapMsg(msg)

		event.WhenSome(func(event Event) {
			s.SendEvent(event)

			processed = true
		})
	})

	return processed
}

// CurrentState returns the current state of the state machine.
func (s *StateMachine[Event, Env]) CurrentState() (State[Event, Env], error) {
	query := stateQuery[Event, Env]{
		CurrentState: make(chan State[Event, Env], 1),
	}

	if !fn.SendOrQuit(s.stateQuery, query, s.quit) {
		return nil, fmt.Errorf("state machine is shutting down")
	}

	return fn.RecvOrTimeout(query.CurrentState, time.Second)
}

// StateSubscriber represents an active subscription to be notified of new
// state transitions.
type StateSubscriber[E any, F Environment] *fn.EventReceiver[State[E, F]]

// RegisterStateEvents registers a new event listener that will be notified of
// new state transitions.
func (s *StateMachine[Event, Env]) RegisterStateEvents() StateSubscriber[
	Event, Env] {

	subscriber := fn.NewEventReceiver[State[Event, Env]](10)

	// TODO(roasbeef): instead give the state and the input event?

	s.newStateEvents.RegisterSubscriber(subscriber)

	return subscriber
}

// RemoveStateSub removes the target state subscriber from the set of active
// subscribers.
func (s *StateMachine[Event, Env]) RemoveStateSub(sub StateSubscriber[
	Event, Env]) {

	_ = s.newStateEvents.RemoveSubscriber(sub)
}

// executeDaemonEvent executes a daemon event, which is a special type of event
// that can be emitted as part of the state transition function of the state
// machine. An error is returned if the type of event is unknown.
func (s *StateMachine[Event, Env]) executeDaemonEvent( //nolint:funlen
	event DaemonEvent) error {

	switch daemonEvent := event.(type) {
	// This is a send message event, so we'll send the event, and also mind
	// any preconditions as well as post-send events.
	case *SendMsgEvent[Event]:
		sendAndCleanUp := func() error {
			log.Debugf("FSM(%v): sending message to target(%v): "+
				"%v", s.cfg.Env.Name(),
				daemonEvent.TargetPeer,
				newLogClosure(func() string {
					return spew.Sdump(daemonEvent.Msgs)
				}),
			)

			err := s.cfg.Daemon.SendMessages(
				daemonEvent.TargetPeer, daemonEvent.Msgs,
			)
			if err != nil {
				return fmt.Errorf("unable to send msgs: %w",
					err)
			}

			// If a post-send event was specified, then we'll
			// funnel that back into the main state machine now as
			// well.
			daemonEvent.PostSendEvent.WhenSome(func(event Event) {
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()

					log.Debugf("FSM(%v): sending "+
						"post-send event: %v",
						s.cfg.Env.Name(),
						newLogClosure(func() string {
							return spew.Sdump(event)
						}),
					)

					s.SendEvent(event)
				}()
			})

			return nil
		}

		// If this doesn't have a SendWhen predicate, then we can just
		// send it off right away.
		if !daemonEvent.SendWhen.IsSome() {
			return sendAndCleanUp()
		}

		// Otherwise, this has a SendWhen predicate, so we'll need
		// launch a goroutine to poll the SendWhen, then send only once
		// the predicate is true.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			predicateTicker := time.NewTicker(pollInterval)
			defer predicateTicker.Stop()

			log.Infof("FSM(%v): waiting for send predicate to "+
				"be true", s.cfg.Env.Name())

			for {
				select {
				case <-predicateTicker.C:
					canSend := fn.MapOptionZ(
						daemonEvent.SendWhen,
						func(pred SendPredicate) bool {
							return pred()
						},
					)

					if canSend {
						log.Infof("FSM(%v): send "+
							"active predicate",
							s.cfg.Env.Name())

						err := sendAndCleanUp()
						if err != nil {
							//nolint:lll
							log.Errorf("FSM(%v): unable to send message: %v", err)
						}

						return
					}

				case <-s.quit:
					return
				}
			}
		}()

		return nil

	// If this is a disable channel event, then we'll disable the channel.
	// This is usually done for things like co-op closes.
	case *DisableChannelEvent:
		log.Debugf("FSM(%v): disabling channel: %v", s.cfg.Env.Name(),
			daemonEvent.ChanPoint)

		err := s.cfg.Daemon.DisableChannel(daemonEvent.ChanPoint)
		if err != nil {
			return fmt.Errorf("unable to disable channel: %w", err)
		}

		return nil

	// If this is a broadcast transaction event, then we'll broadcast with
	// the label attached.
	case *BroadcastTxn:
		log.Debugf("FSM(%v): broadcasting txn, txid=%v",
			s.cfg.Env.Name(), daemonEvent.Tx.TxHash())

		err := s.cfg.Daemon.BroadcastTransaction(
			daemonEvent.Tx, daemonEvent.Label,
		)
		if err != nil {
			return fmt.Errorf("unable to broadcast txn: %w", err)
		}

		return nil

	// The state machine has requested a new event to be sent once a
	// transaction spending a specified outpoint has confirmed.
	case *RegisterSpend[Event]:
		log.Debugf("FSM(%v): registering spend: %v", s.cfg.Env.Name(),
			daemonEvent.OutPoint)

		spendEvent, err := s.cfg.Daemon.RegisterSpendNtfn(
			&daemonEvent.OutPoint, daemonEvent.PkScript,
			daemonEvent.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to register spend: %w", err)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-spendEvent.Spend:
					// If there's a post-send event, then
					// we'll send that into the current
					// state now.
					postSpend := daemonEvent.PostSpendEvent
					postSpend.WhenSome(func(e Event) {
						s.SendEvent(e)
					})

					return

				case <-s.quit:
					return
				}
			}
		}()

		return nil

	// The state machine has requested a new event to be sent once a
	// specified txid+pkScript pair has confirmed.
	case *RegisterConf[Event]:
		log.Debugf("FSM(%v): registering conf: %v", s.cfg.Env.Name(),
			daemonEvent.Txid)

		numConfs := daemonEvent.NumConfs.UnwrapOr(1)
		confEvent, err := s.cfg.Daemon.RegisterConfirmationsNtfn(
			&daemonEvent.Txid, daemonEvent.PkScript,
			numConfs, daemonEvent.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to register conf: %w", err)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-confEvent.Confirmed:
					// If there's a post-conf event, then
					// we'll send that into the current
					// state now.
					//
					// TODO(roasbeef): refactor to
					// dispatchAfterRecv w/ above
					postConf := daemonEvent.PostConfEvent
					postConf.WhenSome(func(e Event) {
						s.SendEvent(e)
					})

					return

				case <-s.quit:
					return
				}
			}
		}()
	}

	return fmt.Errorf("unknown daemon event: %T", event)
}

// applyEvents applies a new event to the state machine. This will continue
// until no further events are emitted by the state machine. Along the way,
// we'll also ensure to execute any daemon events that are emitted.
func (s *StateMachine[Event, Env]) applyEvents(currentState State[Event, Env],
	newEvent Event) (State[Event, Env], error) {

	log.Debugf("FSM(%v): applying new event: %v", s.cfg.Env.Name(),
		newLogClosure(func() string {
			return spew.Sdump(newEvent)
		}),
	)

	eventQueue := fn.NewQueue(newEvent)

	// Given the next event to handle, we'll process the event, then add
	// any new emitted internal events to our event queue. This continues
	// until we reach a terminal state, or we run out of internal events to
	// process.
	//
	//nolint:lll
	for nextEvent := eventQueue.Dequeue(); nextEvent.IsSome(); nextEvent = eventQueue.Dequeue() {
		err := fn.MapOptionZ(nextEvent, func(event Event) error {
			log.Debugf("FSM(%v): processing event: %v",
				s.cfg.Env.Name(),
				newLogClosure(func() string {
					return spew.Sdump(event)
				}),
			)

			// Apply the state transition function of the current
			// state given this new event and our existing env.
			transition, err := currentState.ProcessEvent(
				event, s.cfg.Env,
			)
			if err != nil {
				return err
			}

			newEvents := transition.NewEvents
			err = fn.MapOptionZ(newEvents, func(events EmittedEvent[Event]) error { //nolint:lll
				// With the event processed, we'll process any
				// new daemon events that were emitted as part
				// of this new state transition.
				//
				//nolint:lll
				err := fn.MapOptionZ(events.ExternalEvents, func(dEvents DaemonEventSet) error {
					log.Debugf("FSM(%v): processing "+
						"daemon %v daemon events",
						s.cfg.Env.Name(), len(dEvents))

					for _, dEvent := range dEvents {
						err := s.executeDaemonEvent(
							dEvent,
						)
						if err != nil {
							return err
						}
					}

					return nil
				})
				if err != nil {
					return err
				}

				// Next, we'll add any new emitted events to
				// our event queue.
				//
				//nolint:lll
				events.InternalEvent.WhenSome(func(inEvent Event) {
					log.Debugf("FSM(%v): adding new "+
						"internal event to queue: %v",
						s.cfg.Env.Name(),
						newLogClosure(func() string {
							return spew.Sdump(
								inEvent,
							)
						}),
					)

					eventQueue.Enqueue(inEvent)
				})

				return nil
			})
			if err != nil {
				return err
			}

			log.Infof("FSM(%v): state transition: from_state=%T, "+
				"to_state=%T",
				s.cfg.Env.Name(), currentState,
				transition.NextState)

			// With our events processed, we'll now update our
			// internal state.
			currentState = transition.NextState

			// Notify our subscribers of the new state transition.
			//
			// TODO(roasbeef): will only give us the outer state?
			//  * let FSMs choose which state to emit?
			s.newStateEvents.NotifySubscribers(currentState)

			return nil
		})
		if err != nil {
			return currentState, err
		}
	}

	return currentState, nil
}

// driveMachine is the main event loop of the state machine. It accepts any new
// incoming events, and then drives the state machine forward until it reaches
// a terminal state.
func (s *StateMachine[Event, Env]) driveMachine() {
	defer s.wg.Done()

	log.Debugf("FSM(%v): starting state machine", s.cfg.Env.Name())

	currentState := s.cfg.InitialState

	// Before we start, if we have an init daemon event specified, then
	// we'll handle that now.
	err := fn.MapOptionZ(s.cfg.InitEvent, func(event DaemonEvent) error {
		return s.executeDaemonEvent(event)
	})
	if err != nil {
		log.Errorf("unable to execute init event: %w", err)
		return
	}

	// We just started driving the state machine, so we'll notify our
	// subscribers of this starting state.
	s.newStateEvents.NotifySubscribers(currentState)

	for {
		select {
		// We have a new external event, so we'll drive the state
		// machine forward until we either run out of internal events,
		// or we reach a terminal state.
		case newEvent := <-s.events:
			newState, err := s.applyEvents(currentState, newEvent)
			if err != nil {
				s.cfg.ErrorReporter.ReportError(err)

				log.Errorf("unable to apply event: %v", err)

				// An error occurred, so we'll tear down the
				// entire state machine as we can't proceed.
				go s.Stop()

				return
			}

			currentState = newState

			// If this is a terminal event, then we'll exit the
			// state machine and call any relevant clean up call
			// backs that might have been registered.
			if currentState.IsTerminal() {
				err := s.cfg.Env.CleanUp()
				if err != nil {
					log.Errorf("unable to clean up "+
						"env: %v", err)
				}
			}

		// An outside caller is querying our state, so we'll return the
		// latest state.
		case stateQuery := <-s.stateQuery:
			if !fn.SendOrQuit(
				stateQuery.CurrentState, currentState, s.quit,
			) {

				return
			}

		case <-s.quit:
			// TODO(roasbeef): logs, etc
			//  * something in env?
			return
		}
	}
}