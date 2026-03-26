package protocol

// Control message types matching control_msg.h enum sc_control_msg_type
const (
	ControlMsgTypeInjectKeycode           = 0
	ControlMsgTypeInjectText              = 1
	ControlMsgTypeInjectTouchEvent        = 2
	ControlMsgTypeInjectScrollEvent       = 3
	ControlMsgTypeBackOrScreenOn          = 4
	ControlMsgTypeExpandNotificationPanel = 5
	ControlMsgTypeExpandSettingsPanel     = 6
	ControlMsgTypeCollapsePanels          = 7
	ControlMsgTypeGetClipboard            = 8
	ControlMsgTypeSetClipboard            = 9
	ControlMsgTypeSetDisplayPower         = 10
	ControlMsgTypeRotateDevice            = 11
	ControlMsgTypeUHIDCreate              = 12
	ControlMsgTypeUHIDInput               = 13
	ControlMsgTypeUHIDDestroy             = 14
	ControlMsgTypeOpenHardKeyboardSettings = 15
	ControlMsgTypeStartApp                = 16
	ControlMsgTypeResetVideo              = 17

	ControlMsgMaxSize             = 1 << 18 // 256k
	ControlMsgInjectTextMaxLength = 300
	ControlMsgClipboardMaxLength  = ControlMsgMaxSize - 14

	PointerIDMouse         = ^uint64(0)     // -1
	PointerIDGenericFinger = ^uint64(0) - 1 // -2
	PointerIDVirtualFinger = ^uint64(0) - 2 // -3
)

// ValidateControlMsg performs basic validation on a raw control message from the web client.
// Returns the expected message length, or 0 if invalid/incomplete.
func ValidateControlMsg(data []byte) int {
	if len(data) < 1 {
		return 0
	}

	msgType := data[0]
	switch msgType {
	case ControlMsgTypeInjectKeycode:
		// type(1) + action(1) + keycode(4) + repeat(4) + metastate(4) = 14
		if len(data) < 14 {
			return 0
		}
		return 14

	case ControlMsgTypeInjectText:
		// type(1) + length(4) + text(variable)
		if len(data) < 5 {
			return 0
		}
		textLen := int(data[1])<<24 | int(data[2])<<16 | int(data[3])<<8 | int(data[4])
		total := 5 + textLen
		if textLen > ControlMsgInjectTextMaxLength || len(data) < total {
			return 0
		}
		return total

	case ControlMsgTypeInjectTouchEvent:
		// type(1) + action(1) + pointer_id(8) + position(12) + pressure(2) +
		// action_button(4) + buttons(4) = 32
		if len(data) < 32 {
			return 0
		}
		return 32

	case ControlMsgTypeInjectScrollEvent:
		// type(1) + position(12) + hscroll(2) + vscroll(2) + buttons(4) = 21
		if len(data) < 21 {
			return 0
		}
		return 21

	case ControlMsgTypeBackOrScreenOn, ControlMsgTypeGetClipboard, ControlMsgTypeSetDisplayPower:
		// type(1) + 1 byte payload = 2
		if len(data) < 2 {
			return 0
		}
		return 2

	case ControlMsgTypeSetClipboard:
		// type(1) + sequence(8) + paste(1) + length(4) + text(variable)
		if len(data) < 14 {
			return 0
		}
		textLen := int(data[10])<<24 | int(data[11])<<16 | int(data[12])<<8 | int(data[13])
		total := 14 + textLen
		if textLen > ControlMsgClipboardMaxLength || len(data) < total {
			return 0
		}
		return total

	case ControlMsgTypeExpandNotificationPanel,
		ControlMsgTypeExpandSettingsPanel,
		ControlMsgTypeCollapsePanels,
		ControlMsgTypeRotateDevice,
		ControlMsgTypeOpenHardKeyboardSettings,
		ControlMsgTypeResetVideo:
		// type(1) only
		return 1

	case ControlMsgTypeUHIDCreate:
		// variable, validate minimum
		if len(data) < 8 {
			return 0
		}
		// type(1) + id(2) + vendor(2) + product(2) + name_len(1) + name + desc_size(2) + desc
		nameLen := int(data[7])
		if len(data) < 8+nameLen+2 {
			return 0
		}
		descSize := int(data[8+nameLen])<<8 | int(data[9+nameLen])
		total := 10 + nameLen + descSize
		if len(data) < total {
			return 0
		}
		return total

	case ControlMsgTypeUHIDInput:
		// type(1) + id(2) + size(2) + data
		if len(data) < 5 {
			return 0
		}
		dataSize := int(data[3])<<8 | int(data[4])
		total := 5 + dataSize
		if len(data) < total {
			return 0
		}
		return total

	case ControlMsgTypeUHIDDestroy:
		// type(1) + id(2) = 3
		if len(data) < 3 {
			return 0
		}
		return 3

	case ControlMsgTypeStartApp:
		// type(1) + name_len(1) + name
		if len(data) < 2 {
			return 0
		}
		nameLen := int(data[1])
		total := 2 + nameLen
		if len(data) < total {
			return 0
		}
		return total

	default:
		return -1 // unknown message type
	}
}
