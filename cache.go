package kcache

import (
	"context"
	"fmt"
	"strconv"

	lifecycle "github.com/boz/go-lifecycle"
	logutil "github.com/boz/go-logutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CacheReader interface {
	GetObject(obj metav1.Object) (metav1.Object, error)
	Get(ns string, name string) (metav1.Object, error)
	List() ([]metav1.Object, error)
}

type cache interface {
	CacheReader
	sync([]metav1.Object) []Event
	update(Event) []Event
}

type cacheKey struct {
	namespace string
	name      string
}

type cacheEntry struct {
	version int
	object  metav1.Object
}

type syncRequest struct {
	list     []metav1.Object
	resultch chan<- []Event
}

type getRequest struct {
	key      cacheKey
	resultch chan<- metav1.Object
}

type updateRequest struct {
	evt      Event
	resultch chan<- []Event
}

type _cache struct {
	filter   Filter
	syncch   chan syncRequest
	updatech chan updateRequest

	getch  chan getRequest
	listch chan chan []metav1.Object

	items map[cacheKey]cacheEntry

	log logutil.Log
	lc  lifecycle.Lifecycle
	ctx context.Context
}

func newCache(ctx context.Context, log logutil.Log, stopch <-chan struct{}, filter Filter) cache {
	log = log.WithComponent("cache")

	c := &_cache{
		filter:   filter,
		syncch:   make(chan syncRequest),
		updatech: make(chan updateRequest),
		getch:    make(chan getRequest),
		listch:   make(chan chan []metav1.Object),
		log:      log,
		lc:       lifecycle.New(),
		ctx:      ctx,
	}
	go c.lc.WatchContext(ctx)
	go c.lc.WatchChannel(stopch)
	go c.run()

	return c
}

func (c *_cache) sync(list []metav1.Object) []Event {
	defer c.log.Un(c.log.Trace("sync"))
	resultch := make(chan []Event, 1)
	request := syncRequest{list, resultch}

	select {
	case <-c.lc.ShuttingDown():
		return nil
	case c.syncch <- request:
	}

	return <-resultch
}

func (c *_cache) update(evt Event) []Event {
	defer c.log.Un(c.log.Trace("update"))

	resultch := make(chan []Event, 1)
	request := updateRequest{evt, resultch}

	select {
	case <-c.lc.ShuttingDown():
		return nil
	case c.updatech <- request:
	}

	return <-resultch
}

func (c *_cache) List() ([]metav1.Object, error) {
	defer c.log.Un(c.log.Trace("List"))
	resultch := make(chan []metav1.Object, 1)

	select {
	case <-c.lc.ShuttingDown():
		return nil, fmt.Errorf("not running")
	case c.listch <- resultch:
	}

	return <-resultch, nil
}

func (c *_cache) GetObject(obj metav1.Object) (metav1.Object, error) {
	return c.Get(obj.GetNamespace(), obj.GetName())
}

func (c *_cache) Get(ns, name string) (metav1.Object, error) {
	defer c.log.Un(c.log.Trace("Get"))

	resultch := make(chan metav1.Object, 1)
	key := cacheKey{ns, name}
	request := getRequest{key, resultch}
	select {
	case <-c.lc.ShuttingDown():
		return nil, fmt.Errorf("not running")
	case c.getch <- request:
	}
	return <-resultch, nil
}

func (c *_cache) run() {
	defer c.log.Un(c.log.Trace("run"))

	defer c.lc.ShutdownCompleted()
	for {
		select {
		case request := <-c.syncch:
			request.resultch <- c.doSync(request.list)
		case request := <-c.updatech:
			request.resultch <- c.doUpdate(request.evt)
		case request := <-c.listch:
			request <- c.doList()
		case request := <-c.getch:
			if entry, ok := c.items[request.key]; ok {
				request.resultch <- entry.object
			} else {
				request.resultch <- nil
			}
		case <-c.lc.ShutdownRequest():
			c.lc.ShutdownInitiated()
			return
		}
	}
}

func (c *_cache) doList() []metav1.Object {
	if c.items == nil {
		return nil
	}

	result := make([]metav1.Object, 0, len(c.items))
	for _, obj := range c.items {
		result = append(result, obj.object)
	}
	return result
}

func (c *_cache) doSync(list []metav1.Object) []Event {
	defer c.log.Un(c.log.Trace("doSync"))

	var result []Event

	if c.items == nil {
		c.doInitialSync(list)
		return result
	}

	result, err := c.processList(list)
	if err != nil {
		c.log.ErrWarn(err, "cache.processList()")
	}
	return result
}

func (c *_cache) doUpdate(evt Event) []Event {
	defer c.log.Un(c.log.Trace("doUpdate"))

	events := make([]Event, 0, 1)

	obj := evt.Resource()

	version, err := strconv.Atoi(obj.GetResourceVersion())
	if err != nil {
		c.log.ErrWarn(err, "resource version %v", obj.GetResourceVersion())
		return events
	}

	key := cacheKey{obj.GetNamespace(), obj.GetName()}
	entry := cacheEntry{version, obj}

	current, found := c.items[key]

	accept := c.filter.Accept(entry.object)

	switch evt.Type() {
	case EventTypeDelete:
		if found {
			events = append(events, evt)
			delete(c.items, key)
		}
	default:
		switch {
		case accept && !found:
			events = append(events, NewEvent(EventTypeCreate, obj))
			c.items[key] = entry
		case accept && current.version < entry.version:
			events = append(events, NewEvent(EventTypeUpdate, obj))
			c.items[key] = entry
		case !accept && current.version < entry.version:
			events = append(events, NewEvent(EventTypeDelete, obj))
			delete(c.items, key)
		case current.version >= entry.version:
			c.log.Debugf("skipping version %v > %v", current.version, entry.version)
		}
	}

	return events
}

func (c *_cache) doInitialSync(list []metav1.Object) {
	c.items = make(map[cacheKey]cacheEntry, len(list))

	for _, obj := range list {
		key, err := c.createKey(obj)
		if err != nil {
			c.log.ErrWarn(err, "createKey(%T)", obj)
			continue
		}

		entry, err := c.createEntry(obj)
		if err != nil {
			c.log.ErrWarn(err, "createEntry(%T)", obj)
			continue
		}

		if !c.filter.Accept(entry.object) {
			continue
		}

		c.items[key] = entry
	}
}

func (c *_cache) processList(list []metav1.Object) ([]Event, error) {

	var events []Event
	set := make(map[cacheKey]cacheEntry)

	for _, obj := range list {
		key, err := c.createKey(obj)
		if err != nil {
			c.log.ErrWarn(err, "createKey(%T)", obj)
			continue
		}

		entry, err := c.createEntry(obj)
		if err != nil {
			c.log.ErrWarn(err, "createEntry(%T)", obj)
			continue
		}

		current, found := c.items[key]

		accept := c.filter.Accept(entry.object)

		switch {
		case accept && !found:
			events = append(events, NewEvent(EventTypeCreate, entry.object))
		case accept && current.version < entry.version:
			events = append(events, NewEvent(EventTypeUpdate, entry.object))
			c.items[key] = entry
		case current.version >= entry.version:
			c.log.Debugf("skipping version %v > %v", current.version, entry.version)
		default:
			// don't add to working new working set of objects
			continue
		}

		set[key] = entry
	}

	for k, current := range c.items {
		if _, ok := set[k]; !ok {
			events = append(events, NewEvent(EventTypeDelete, current.object))
		}
	}

	return events, nil
}

func (c *_cache) createKey(obj metav1.Object) (cacheKey, error) {
	ns := obj.GetNamespace()
	name := obj.GetName()

	return cacheKey{ns, name}, nil
}

func (c *_cache) createEntry(obj metav1.Object) (cacheEntry, error) {
	version, err := strconv.Atoi(obj.GetResourceVersion())
	if err != nil {
		return cacheEntry{}, err
	}
	return cacheEntry{version, obj}, nil
}
