package ui

import (
	"image"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows UI Automation provider for the window's current Gio frame.
const (
	uiaNotAvailable = 0x80040201
	uiaNotEnabled   = 0x80040200
	uiaNoInterface  = 0x80004002
	uiaOutOfMemory  = 0x8007000e
	uiaInvalidArg   = 0x80070057

	uiaInvokePattern = 10000
	uiaValuePattern  = 10002
	uiaTogglePattern = 10015

	wmGetObject   = 0x003d
	wmNCDestroy   = 0x0082
	uiaRootObject = -25
)

var (
	uiaCore             = windows.NewLazySystemDLL("UIAutomationCore.dll")
	uiaReturnProvider   = uiaCore.NewProc("UiaReturnRawElementProvider")
	uiaHostProvider     = uiaCore.NewProc("UiaHostProviderFromHwnd")
	uiaClientsListening = uiaCore.NewProc("UiaClientsAreListening")
	uiaRaiseEvent       = uiaCore.NewProc("UiaRaiseAutomationEvent")
	uiaRaiseStructure   = uiaCore.NewProc("UiaRaiseStructureChangedEvent")
	uiaDisconnect       = uiaCore.NewProc("UiaDisconnectProvider")
	oleAutomation       = windows.NewLazySystemDLL("oleaut32.dll")
	uiaAllocString      = oleAutomation.NewProc("SysAllocStringLen")
	uiaArrayCreate      = oleAutomation.NewProc("SafeArrayCreateVector")
	uiaArrayPut         = oleAutomation.NewProc("SafeArrayPutElement")
	uiaControls         = windows.NewLazySystemDLL("comctl32.dll")
	uiaSetSubclass      = uiaControls.NewProc("SetWindowSubclass")
	uiaRemoveSubclass   = uiaControls.NewProc("RemoveWindowSubclass")
	uiaDefSubclass      = uiaControls.NewProc("DefSubclassProc")
	uiaClientToScreen   = windows.NewLazySystemDLL("user32.dll").NewProc("ClientToScreen")
	uiaForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("GetForegroundWindow")
	uiaSubclassCallback uintptr
	uiaPointCallback    = syscall.NewCallback(elementFromPoint)
	accessWindows       sync.Map // HWND to *accessWindow
	liveProviders       sync.Map // keeps pinned COM objects alive
)

type accessWindow struct {
	frame accessFrame
	ui    *ui
	hwnd  uintptr
	root  *uiaProvider
	done  chan struct{}
	event chan accessEvent
}

type accessEvent struct {
	structure bool
	focus     int32
}

func attachAccessibility(u *ui, hwnd uintptr) *accessWindow {
	w := &accessWindow{ui: u, hwnd: hwnd, done: make(chan struct{}), event: make(chan accessEvent, 1)}
	w.frame.targets = make(map[int32]accessNode)
	w.frame.keys, w.frame.priorKeys = make(map[accessKey]int32), make(map[accessKey]int32)
	w.root = newProvider(w, 0)
	accessWindows.Store(hwnd, w)
	if result, _, _ := uiaSetSubclass.Call(hwnd, uiaSubclassCallback, 1, 0); result == 0 {
		accessWindows.Delete(hwnd)
		releaseProvider(&w.root.simple)
		return nil
	}
	go w.raiseEvents()
	return w
}

func accessWindowProc(hwnd, msg, wParam, lParam, id, _ uintptr) uintptr {
	value, ok := accessWindows.Load(hwnd)
	if ok {
		w := value.(*accessWindow)
		switch {
		case msg == wmGetObject && int32(lParam) == uiaRootObject:
			result, _, _ := syscall.SyscallN(uiaReturnProvider.Addr(), hwnd, wParam, lParam, uintptr(unsafe.Pointer(&w.root.simple)))
			return result
		case msg == wmNCDestroy:
			w.frame.tree.Store(nil)
			close(w.done)
			accessWindows.Delete(hwnd)
			uiaRemoveSubclass.Call(hwnd, uiaSubclassCallback, id)
			if uiaDisconnect.Find() == nil {
				syscall.SyscallN(uiaDisconnect.Addr(), uintptr(unsafe.Pointer(&w.root.simple)))
			}
			uiaReturnProvider.Call(hwnd, 0, 0, 0)
			releaseProvider(&w.root.simple)
		}
	}
	result, _, _ := uiaDefSubclass.Call(hwnd, msg, wParam, lParam)
	return result
}

func (w *accessWindow) notify(changed bool, focus int32) {
	if (!changed && focus == 0) || !uiaListening() {
		return
	}
	event := accessEvent{structure: changed, focus: focus}
	select {
	case prior := <-w.event:
		event.structure = event.structure || prior.structure
		if event.focus == 0 {
			event.focus = prior.focus
		}
	default:
	}
	w.event <- event
}

func uiaListening() bool {
	listening, _, _ := uiaClientsListening.Call()
	return listening != 0
}

func (w *accessWindow) raiseEvents() {
	for {
		select {
		case <-w.done:
			return
		case event := <-w.event:
			tree := w.frame.tree.Load()
			if tree == nil {
				return
			}
			if event.structure {
				root := newProvider(w, 0)
				// StructureChangeType_ChildrenInvalidated.
				syscall.SyscallN(uiaRaiseStructure.Addr(), uintptr(unsafe.Pointer(&root.simple)), 2, 0, 0)
				releaseProvider(&root.simple)
			}
			if event.focus != 0 && event.focus == tree.focus && w.foreground() {
				focus := newProvider(w, event.focus)
				syscall.SyscallN(uiaRaiseEvent.Addr(), uintptr(unsafe.Pointer(&focus.simple)), 20005) // AutomationFocusChanged
				releaseProvider(&focus.simple)
			}
		}
	}
}

func (w *accessWindow) foreground() bool {
	hwnd, _, _ := uiaForegroundWindow.Call()
	return hwnd == w.hwnd
}

func (w *accessWindow) clientOrigin() image.Point {
	var point struct{ x, y int32 }
	syscall.SyscallN(uiaClientToScreen.Addr(), w.hwnd, uintptr(unsafe.Pointer(&point)))
	return image.Pt(int(point.x), int(point.y))
}

// Each COM interface has the standard vtable pointer as its first field.
type uiaFace struct {
	vtable *uintptr
	p      *uiaProvider
}

type uiaProvider struct {
	simple, fragment, root, invoke, value, toggle uiaFace
	w                                             *accessWindow
	id                                            int32
	refs                                          atomic.Int32
	pin                                           runtime.Pinner
}

func newProvider(w *accessWindow, id int32) *uiaProvider {
	p := &uiaProvider{w: w, id: id}
	p.simple = uiaFace{&uiaSimpleTable[0], p}
	p.fragment = uiaFace{&uiaFragmentTable[0], p}
	p.root = uiaFace{&uiaRootTable[0], p}
	p.invoke = uiaFace{&uiaInvokeTable[0], p}
	p.value = uiaFace{&uiaValueTable[0], p}
	p.toggle = uiaFace{&uiaToggleTable[0], p}
	p.refs.Store(1)
	p.pin.Pin(p)
	liveProviders.Store(p, struct{}{})
	return p
}

func addRef(face *uiaFace) uintptr { return uintptr(face.p.refs.Add(1)) }

func releaseProvider(face *uiaFace) uintptr {
	p := face.p
	refs := p.refs.Add(-1)
	if refs == 0 {
		liveProviders.Delete(p)
		p.pin.Unpin()
	}
	return uintptr(refs)
}

func iid(s string) windows.GUID {
	id, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return id
}

var (
	iidUnknown  = iid("{00000000-0000-0000-C000-000000000046}")
	iidSimple   = iid("{d6dd68d1-86fd-4332-8666-9abedea2d24c}")
	iidFragment = iid("{f7063da8-8359-439c-9297-bbc5299a7d87}")
	iidRoot     = iid("{620ce2a5-ab8f-40a9-86cb-de3c75599b58}")
	iidInvoke   = iid("{54fcb24b-e18e-47a2-b4d3-eccbe77599a2}")
	iidValue    = iid("{c7935180-6fb3-4201-b174-7df73adbf64a}")
	iidToggle   = iid("{56d00bd0-c4f4-433c-a836-1a52a57e0892}")
)

func queryInterface(face *uiaFace, id *windows.GUID, out **uiaFace) uintptr {
	p := face.p
	*out = nil
	switch *id {
	case iidUnknown, iidSimple:
		*out = &p.simple
	case iidFragment:
		*out = &p.fragment
	case iidRoot:
		if p.id == 0 {
			*out = &p.root
		}
	case iidInvoke:
		*out = p.pattern(uiaInvokePattern)
	case iidValue:
		*out = p.pattern(uiaValuePattern)
	case iidToggle:
		*out = p.pattern(uiaTogglePattern)
	}
	if *out == nil {
		return uiaNoInterface
	}
	addRef(face)
	return 0
}

func (p *uiaProvider) node() (accessNode, bool) {
	if tree := p.w.frame.tree.Load(); tree != nil {
		node, ok := tree.nodes[p.id]
		return node, ok
	}
	return accessNode{}, false
}

func (p *uiaProvider) pattern(pattern int32) *uiaFace {
	node, _ := p.node()
	switch {
	case pattern == uiaInvokePattern && node.invoke != nil && node.role != accessCheck:
		return &p.invoke
	case pattern == uiaValuePattern && node.setValue != nil:
		return &p.value
	case pattern == uiaTogglePattern && node.role == accessCheck:
		return &p.toggle
	}
	return nil
}

func getPattern(face *uiaFace, pattern int32, out **uiaFace) uintptr {
	*out = face.p.pattern(pattern)
	if *out != nil {
		addRef(face)
	}
	return 0
}

type uiaVariant struct {
	kind uint16
	_    [3]uint16
	data [2]uintptr
}

func (v *uiaVariant) integer(value int32) { v.kind, v.data[0] = 3, uintptr(value) }

func (v *uiaVariant) boolean(value bool) {
	v.kind = 11
	if value {
		v.data[0] = 0xffff
	}
}

func (v *uiaVariant) string(value string) uintptr {
	v.kind = 8
	return allocBSTR(value, &v.data[0])
}

func allocBSTR(value string, out *uintptr) uintptr {
	text, err := windows.UTF16FromString(value)
	if err != nil {
		return uiaInvalidArg
	}
	*out, _, _ = syscall.SyscallN(uiaAllocString.Addr(), uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)-1))
	runtime.KeepAlive(text)
	if *out == 0 {
		return uiaOutOfMemory
	}
	return 0
}

func getProperty(face *uiaFace, property int32, out *uiaVariant) uintptr {
	*out = uiaVariant{}
	node, ok := face.p.node()
	if !ok {
		return uiaNotAvailable
	}
	switch property {
	case 30003: // ControlType
		out.integer(node.role)
	case 30005: // Name
		return out.string(node.name)
	case 30008: // HasKeyboardFocus
		out.boolean(node.focused && face.p.w.foreground())
	case 30009: // IsKeyboardFocusable
		out.boolean(node.tag != nil && !node.disabled)
	case 30010: // IsEnabled
		out.boolean(!node.disabled)
	case 30016, 30017: // IsControlElement, IsContentElement
		out.boolean(true)
	case 30022: // IsOffscreen
		out.boolean(node.bounds.Empty())
	case 30024: // FrameworkId
		return out.string("Gio")
	}
	return 0
}

func hostProvider(face *uiaFace, out *uintptr) uintptr {
	*out = 0
	if face.p.id != 0 {
		return 0
	}
	result, _, _ := syscall.SyscallN(uiaHostProvider.Addr(), face.p.w.hwnd, uintptr(unsafe.Pointer(out)))
	return result
}

func navigate(face *uiaFace, direction int32, out **uiaFace) uintptr {
	*out = nil
	p := face.p
	tree := p.w.frame.tree.Load()
	if tree == nil {
		return uiaNotAvailable
	}
	if p.id == 0 {
		if len(tree.order) != 0 {
			switch direction {
			case 3: // FirstChild
				*out = &newProvider(p.w, tree.order[0]).fragment
			case 4: // LastChild
				*out = &newProvider(p.w, tree.order[len(tree.order)-1]).fragment
			}
		}
		return 0
	}
	index := slices.Index(tree.order, p.id)
	if index < 0 {
		return uiaNotAvailable
	}
	switch direction {
	case 0: // Parent
		*out = &newProvider(p.w, 0).fragment
	case 1: // NextSibling
		if index+1 < len(tree.order) {
			*out = &newProvider(p.w, tree.order[index+1]).fragment
		}
	case 2: // PreviousSibling
		if index > 0 {
			*out = &newProvider(p.w, tree.order[index-1]).fragment
		}
	}
	return 0
}

func runtimeID(face *uiaFace, out *uintptr) uintptr {
	*out = 0
	if face.p.id == 0 {
		return 0
	}
	array, _, _ := uiaArrayCreate.Call(3, 0, 2) // VT_I4
	if array == 0 {
		return uiaOutOfMemory
	}
	for index, value := range [2]int32{3, face.p.id} { // UiaAppendRuntimeId
		i := int32(index)
		syscall.SyscallN(uiaArrayPut.Addr(), array, uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&value)))
	}
	*out = array
	return 0
}

type uiaRect struct{ x, y, width, height float64 }

func boundingRect(face *uiaFace, out *uiaRect) uintptr {
	node, ok := face.p.node()
	if !ok {
		return uiaNotAvailable
	}
	rect := node.bounds.Add(face.p.w.clientOrigin())
	*out = uiaRect{float64(rect.Min.X), float64(rect.Min.Y), float64(rect.Dx()), float64(rect.Dy())}
	return 0
}

func elementFromPoint(face *uiaFace, x, y uint64, out **uiaFace) uintptr {
	*out = nil
	p := face.p
	tree := p.w.frame.tree.Load()
	if tree == nil {
		return uiaNotAvailable
	}
	point := image.Pt(int(math.Float64frombits(x)), int(math.Float64frombits(y))).Sub(p.w.clientOrigin())
	if !point.In(tree.nodes[0].bounds) {
		return 0
	}
	for i := len(tree.order) - 1; i >= 0; i-- {
		id := tree.order[i]
		if point.In(tree.nodes[id].bounds) {
			*out = &newProvider(p.w, id).fragment
			return 0
		}
	}
	*out = &newProvider(p.w, 0).fragment
	return 0
}

func getFocus(face *uiaFace, out **uiaFace) uintptr {
	*out = nil
	if tree := face.p.w.frame.tree.Load(); tree != nil && face.p.w.foreground() {
		*out = &newProvider(face.p.w, tree.focus).fragment
	}
	return 0
}

func (p *uiaProvider) perform(action func(accessNode)) uintptr {
	node, ok := p.node()
	if !ok {
		return uiaNotAvailable
	}
	if node.disabled {
		return uiaNotEnabled
	}
	p.w.ui.post(func() {
		if current, ok := p.node(); ok && !current.disabled {
			action(current)
		}
	})
	return 0
}

func invokeElement(face *uiaFace) uintptr {
	return face.p.perform(func(node accessNode) {
		if node.invoke != nil {
			node.invoke()
		}
	})
}

func setFocus(face *uiaFace) uintptr {
	return face.p.perform(func(accessNode) { face.p.w.frame.focus = face.p.id })
}

func fragmentRoot(face *uiaFace, out **uiaFace) uintptr {
	*out = &newProvider(face.p.w, 0).root
	return 0
}

func setElementValue(face *uiaFace, value *uint16) uintptr {
	text := windows.UTF16PtrToString(value)
	return face.p.perform(func(node accessNode) {
		if node.setValue != nil {
			node.setValue(text)
		}
	})
}

func getElementValue(face *uiaFace, out *uintptr) uintptr {
	node, ok := face.p.node()
	if !ok {
		return uiaNotAvailable
	}
	return allocBSTR(node.value, out)
}

func getToggleState(face *uiaFace, out *int32) uintptr {
	node, ok := face.p.node()
	if !ok {
		return uiaNotAvailable
	}
	*out = 0
	if node.checked {
		*out = 1
	}
	return 0
}

func vtable(methods ...any) []uintptr {
	table := []uintptr{syscall.NewCallback(queryInterface), syscall.NewCallback(addRef), syscall.NewCallback(releaseProvider)}
	for _, method := range methods {
		table = append(table, syscall.NewCallback(method))
	}
	return table
}

// Install tables in init because callbacks construct providers that refer to them.
var uiaSimpleTable, uiaFragmentTable, uiaRootTable, uiaInvokeTable, uiaValueTable, uiaToggleTable []uintptr

func init() {
	uiaSubclassCallback = syscall.NewCallback(accessWindowProc)
	uiaSimpleTable = vtable(
		func(_ *uiaFace, out *int32) uintptr { *out = 2; return 0 }, // ServerSideProvider
		getPattern, getProperty, hostProvider)
	uiaFragmentTable = vtable(navigate, runtimeID, boundingRect,
		func(_ *uiaFace, out *uintptr) uintptr { *out = 0; return 0 }, setFocus, fragmentRoot)
	uiaRootTable = vtable(getFocus)
	uiaRootTable = append(uiaRootTable[:3], uiaPointStubAddress(), uiaRootTable[3])
	uiaInvokeTable = vtable(invokeElement)
	uiaValueTable = vtable(setElementValue, getElementValue, func(_ *uiaFace, out *int32) uintptr { *out = 0; return 0 })
	uiaToggleTable = vtable(invokeElement, getToggleState)
}

func uiaPointStubAddress() uintptr
