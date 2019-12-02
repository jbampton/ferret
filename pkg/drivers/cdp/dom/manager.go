package dom

import (
	"context"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/input"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/values"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"sync"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/dom"
	"github.com/mafredri/cdp/rpcc"

	"github.com/MontFerret/ferret/pkg/drivers/cdp/events"
)

var (
	eventDocumentUpdated       = events.New("doc_updated")
	eventAttrModified          = events.New("attr_modified")
	eventAttrRemoved           = events.New("attr_removed")
	eventChildNodeCountUpdated = events.New("child_count_updated")
	eventChildNodeInserted     = events.New("child_inserted")
	eventChildNodeRemoved      = events.New("child_removed")
)

type (
	DocumentUpdatedListener func(ctx context.Context)

	AttrModifiedListener func(ctx context.Context, nodeID dom.NodeID, name, value string)

	AttrRemovedListener func(ctx context.Context, nodeID dom.NodeID, name string)

	ChildNodeCountUpdatedListener func(ctx context.Context, nodeID dom.NodeID, count int)

	ChildNodeInsertedListener func(ctx context.Context, nodeID, previousNodeID dom.NodeID, node dom.Node)

	ChildNodeRemovedListener func(ctx context.Context, nodeID, previousNodeID dom.NodeID)

	Frame struct {
		tree page.FrameTree
		node *HTMLDocument
	}

	Manager struct {
		mu        sync.Mutex
		logger    *zerolog.Logger
		client    *cdp.Client
		events    *events.Loop
		mouse     *input.Mouse
		keyboard  *input.Keyboard
		mainFrame page.FrameID
		frames    map[page.FrameID]Frame
		cancel    context.CancelFunc
	}
)

func New(
	logger *zerolog.Logger,
	client *cdp.Client,
	eventLoop *events.Loop,
	mouse *input.Mouse,
	keyboard *input.Keyboard,
) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	onDocUpdated, err := client.DOM.DocumentUpdated(ctx)

	if err != nil {
		return nil, err
	}

	onAttrModified, err := client.DOM.AttributeModified(ctx)

	if err != nil {
		onDocUpdated.Close()
		return nil, err
	}

	onAttrRemoved, err := client.DOM.AttributeRemoved(ctx)

	if err != nil {
		onDocUpdated.Close()
		onAttrModified.Close()
		return nil, err
	}

	onChildCountUpdated, err := client.DOM.ChildNodeCountUpdated(ctx)

	if err != nil {
		onDocUpdated.Close()
		onAttrModified.Close()
		onAttrRemoved.Close()
		return nil, err
	}

	onChildNodeInserted, err := client.DOM.ChildNodeInserted(ctx)

	if err != nil {
		onDocUpdated.Close()
		onAttrModified.Close()
		onAttrRemoved.Close()
		onChildCountUpdated.Close()
		return nil, err
	}

	onChildNodeRemoved, err := client.DOM.ChildNodeRemoved(ctx)

	if err != nil {
		onDocUpdated.Close()
		onAttrModified.Close()
		onAttrRemoved.Close()
		onChildCountUpdated.Close()
		onChildNodeInserted.Close()
		return nil, err
	}

	eventLoop.AddSource(events.NewSource(eventDocumentUpdated, onDocUpdated, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.DocumentUpdatedClient).Recv()
	}))

	eventLoop.AddSource(events.NewSource(eventAttrModified, onAttrModified, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.AttributeModifiedClient).Recv()
	}))

	eventLoop.AddSource(events.NewSource(eventAttrRemoved, onAttrRemoved, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.AttributeRemovedClient).Recv()
	}))

	eventLoop.AddSource(events.NewSource(eventChildNodeCountUpdated, onChildCountUpdated, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.ChildNodeCountUpdatedClient).Recv()
	}))

	eventLoop.AddSource(events.NewSource(eventChildNodeInserted, onChildNodeInserted, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.ChildNodeInsertedClient).Recv()
	}))

	eventLoop.AddSource(events.NewSource(eventChildNodeRemoved, onChildNodeRemoved, func(stream rpcc.Stream) (i interface{}, e error) {
		return stream.(dom.ChildNodeRemovedClient).Recv()
	}))

	m := new(Manager)
	m.logger = logger
	m.client = client
	m.events = eventLoop
	m.mouse = mouse
	m.keyboard = keyboard
	m.frames = make(map[page.FrameID]Frame)
	m.cancel = cancel

	return m, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	errs := make([]error, 0, len(m.frames))

	for _, f := range m.frames {
		// if initialized
		if f.node != nil {
			if err := f.node.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}

	m.frames = make(map[page.FrameID]Frame)

	return core.Errors(errs...)
}

func (m *Manager) GetMainFrame() *HTMLDocument {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mainFrame == "" {
		return nil
	}

	mainFrame, exists := m.frames[m.mainFrame]

	if exists {
		return mainFrame.node
	}

	return nil
}

func (m *Manager) SetMainFrame(doc *HTMLDocument) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mainFrame != "" {
		if err := m.removeFrameRecursively(m.mainFrame); err != nil {
			m.logger.Error().Err(err).Msg("failed to close previous main frame")
		}
	}

	m.mainFrame = doc.frameTree.Frame.ID

	m.addPreloadedFrame(doc)
}

func (m *Manager) AddFrame(frame page.FrameTree) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.addFrameInternal(frame)
}

func (m *Manager) RemoveFrame(frameID page.FrameID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.removeFrameInternal(frameID)
}

func (m *Manager) RemoveFramesByParentID(parentFrameID page.FrameID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	frame, found := m.frames[parentFrameID]

	if !found {
		return errors.New("frame not found")
	}

	for _, child := range frame.tree.ChildFrames {
		if err := m.removeFrameRecursively(child.Frame.ID); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) GetFrame(ctx context.Context, frameID page.FrameID) (*HTMLDocument, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.getFrameInternal(ctx, frameID)
}

func (m *Manager) GetFrames(ctx context.Context) (*values.Array, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	arr := values.NewArray(len(m.frames))

	for _, f := range m.frames {
		doc, err := m.getFrameInternal(ctx, f.tree.Frame.ID)

		if err != nil {
			return nil, err
		}

		arr.Push(doc)

	}

	return arr, nil
}

func (m *Manager) AddDocumentUpdatedListener(listener DocumentUpdatedListener) events.ListenerID {
	return m.events.AddListener(eventDocumentUpdated, func(ctx context.Context, _ interface{}) bool {
		listener(ctx)

		return true
	})
}

func (m *Manager) RemoveReloadListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventDocumentUpdated, listenerID)
}

func (m *Manager) AddAttrModifiedListener(listener AttrModifiedListener) events.ListenerID {
	return m.events.AddListener(eventAttrModified, func(ctx context.Context, message interface{}) bool {
		reply := message.(dom.AttributeModifiedReply)

		listener(ctx, reply.NodeID, reply.Name, reply.Value)

		return true
	})
}

func (m *Manager) RemoveAttrModifiedListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventAttrModified, listenerID)
}

func (m *Manager) AddAttrRemovedListener(listener AttrRemovedListener) events.ListenerID {
	return m.events.AddListener(eventAttrRemoved, func(ctx context.Context, message interface{}) bool {
		reply := message.(dom.AttributeRemovedReply)

		listener(ctx, reply.NodeID, reply.Name)

		return true
	})
}

func (m *Manager) RemoveAttrRemovedListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventAttrRemoved, listenerID)
}

func (m *Manager) AddChildNodeCountUpdatedListener(listener ChildNodeCountUpdatedListener) events.ListenerID {
	return m.events.AddListener(eventChildNodeCountUpdated, func(ctx context.Context, message interface{}) bool {
		reply := message.(dom.ChildNodeCountUpdatedReply)

		listener(ctx, reply.NodeID, reply.ChildNodeCount)

		return true
	})
}

func (m *Manager) RemoveChildNodeCountUpdatedListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventChildNodeCountUpdated, listenerID)
}

func (m *Manager) AddChildNodeInsertedListener(listener ChildNodeInsertedListener) events.ListenerID {
	return m.events.AddListener(eventChildNodeInserted, func(ctx context.Context, message interface{}) bool {
		reply := message.(dom.ChildNodeInsertedReply)

		listener(ctx, reply.ParentNodeID, reply.PreviousNodeID, reply.Node)

		return true
	})
}

func (m *Manager) RemoveChildNodeInsertedListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventChildNodeInserted, listenerID)
}

func (m *Manager) AddChildNodeRemovedListener(listener ChildNodeRemovedListener) events.ListenerID {
	return m.events.AddListener(eventChildNodeRemoved, func(ctx context.Context, message interface{}) bool {
		reply := message.(dom.ChildNodeRemovedReply)

		listener(ctx, reply.ParentNodeID, reply.NodeID)

		return true
	})
}

func (m *Manager) RemoveChildNodeRemovedListener(listenerID events.ListenerID) {
	m.events.RemoveListener(eventChildNodeRemoved, listenerID)
}

func (m *Manager) WaitForDOMReady(ctx context.Context) error {
	onDOMReady, err := m.client.Page.DOMContentEventFired(ctx)

	if err != nil {
		return err
	}

	_, err = onDOMReady.Recv()

	if err != nil {
		return err
	}

	return nil
}

func (m *Manager) addFrameInternal(frame page.FrameTree) {
	m.frames[frame.Frame.ID] = Frame{
		tree: frame,
		node: nil,
	}

	for _, child := range frame.ChildFrames {
		m.addFrameInternal(child)
	}
}

func (m *Manager) addPreloadedFrame(doc *HTMLDocument) {
	m.frames[doc.frameTree.Frame.ID] = Frame{
		tree: doc.frameTree,
		node: doc,
	}

	for _, child := range doc.frameTree.ChildFrames {
		m.addFrameInternal(child)
	}
}

func (m *Manager) getFrameInternal(ctx context.Context, frameID page.FrameID) (*HTMLDocument, error) {
	frame, found := m.frames[frameID]

	if !found {
		return nil, errors.New("frame not found")
	}

	// frame is initialized
	if frame.node != nil {
		return frame.node, nil
	}

	// the frames is not loaded yet
	node, execID, err := resolveFrame(ctx, m.client, frameID)

	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve frame node")
	}

	doc, err := LoadHTMLDocument(
		ctx,
		m.logger,
		m.client,
		m,
		m.mouse,
		m.keyboard,
		node,
		frame.tree,
		execID,
	)

	if err != nil {
		return nil, errors.Wrap(err, "failed to load frame document")
	}

	frame.node = doc

	return doc, nil
}

func (m *Manager) removeFrameInternal(frameID page.FrameID) error {
	current, exists := m.frames[frameID]

	if !exists {
		return core.Error(core.ErrNotFound, "frame")
	}

	delete(m.frames, frameID)

	if current.node == nil {
		return nil
	}

	return current.node.Close()
}

func (m *Manager) removeFrameRecursively(frameID page.FrameID) error {
	parent, exists := m.frames[frameID]

	if !exists {
		return core.Error(core.ErrNotFound, "frame")
	}

	for _, child := range parent.tree.ChildFrames {
		if err := m.removeFrameRecursively(child.Frame.ID); err != nil {
			return err
		}
	}

	return m.removeFrameInternal(frameID)
}
