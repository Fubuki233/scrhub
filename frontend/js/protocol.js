/**
 * Control message binary encoder for web-scrcpy.
 * Matches the exact formats from scrcpy's control_msg.c sc_control_msg_serialize().
 * All multi-byte fields are big-endian.
 */

// Message types (matching control_msg.h enum)
const MSG_TYPE_INJECT_KEYCODE = 0;
const MSG_TYPE_INJECT_TEXT = 1;
const MSG_TYPE_INJECT_TOUCH_EVENT = 2;
const MSG_TYPE_INJECT_SCROLL_EVENT = 3;
const MSG_TYPE_BACK_OR_SCREEN_ON = 4;
const MSG_TYPE_EXPAND_NOTIFICATION_PANEL = 5;
const MSG_TYPE_EXPAND_SETTINGS_PANEL = 6;
const MSG_TYPE_COLLAPSE_PANELS = 7;
const MSG_TYPE_GET_CLIPBOARD = 8;
const MSG_TYPE_SET_CLIPBOARD = 9;
const MSG_TYPE_SET_DISPLAY_POWER = 10;
const MSG_TYPE_ROTATE_DEVICE = 11;

// Android KeyEvent actions
const AKEY_ACTION_DOWN = 0;
const AKEY_ACTION_UP = 1;

// Android MotionEvent actions
const AMOTION_ACTION_DOWN = 0;
const AMOTION_ACTION_UP = 1;
const AMOTION_ACTION_MOVE = 2;

// Pointer IDs
const POINTER_ID_MOUSE = 0xFFFFFFFFFFFFFFFFn;  // -1 as uint64
const POINTER_ID_FINGER = 0xFFFFFFFFFFFFFFFEn; // -2 as uint64

/**
 * Convert a float [0, 1] to unsigned 16-bit fixed point.
 */
function floatToU16fp(f) {
    const scaled = Math.max(0, Math.min(1, f)) * 0xFFFF;
    return Math.round(scaled) & 0xFFFF;
}

/**
 * Convert a float [-1, 1] to signed 16-bit fixed point.
 */
function floatToI16fp(f) {
    const clamped = Math.max(-1, Math.min(1, f));
    if (clamped >= 0) {
        return Math.round(clamped * 0x7FFF) & 0xFFFF;
    } else {
        return (Math.round(clamped * 0x8000) & 0xFFFF);
    }
}

/**
 * INJECT_KEYCODE: 14 bytes
 * [type(1)][action(1)][keycode(4)][repeat(4)][metastate(4)]
 */
function serializeInjectKeycode(action, keycode, repeat, metastate) {
    const buf = new ArrayBuffer(14);
    const view = new DataView(buf);
    view.setUint8(0, MSG_TYPE_INJECT_KEYCODE);
    view.setUint8(1, action);
    view.setUint32(2, keycode);
    view.setUint32(6, repeat || 0);
    view.setUint32(10, metastate || 0);
    return new Uint8Array(buf);
}

/**
 * INJECT_TOUCH_EVENT: 32 bytes
 * [type(1)][action(1)][pointer_id(8)][x(4)][y(4)][w(2)][h(2)]
 * [pressure(2)][action_button(4)][buttons(4)]
 */
function serializeInjectTouch(action, pointerId, x, y, screenW, screenH, pressure, actionButton, buttons) {
    const buf = new ArrayBuffer(32);
    const view = new DataView(buf);
    view.setUint8(0, MSG_TYPE_INJECT_TOUCH_EVENT);
    view.setUint8(1, action);
    // pointer_id as uint64 big-endian
    view.setUint32(2, Number(pointerId >> BigInt(32)));
    view.setUint32(6, Number(pointerId & BigInt(0xFFFFFFFF)));
    // position
    view.setInt32(10, x);
    view.setInt32(14, y);
    view.setUint16(18, screenW);
    view.setUint16(20, screenH);
    // pressure as u16 fixed-point
    view.setUint16(22, floatToU16fp(pressure));
    view.setUint32(24, actionButton || 0);
    view.setUint32(28, buttons || 0);
    return new Uint8Array(buf);
}

/**
 * INJECT_SCROLL_EVENT: 21 bytes
 * [type(1)][x(4)][y(4)][w(2)][h(2)][hscroll(2)][vscroll(2)][buttons(4)]
 */
function serializeInjectScroll(x, y, screenW, screenH, hscroll, vscroll, buttons) {
    const buf = new ArrayBuffer(21);
    const view = new DataView(buf);
    view.setUint8(0, MSG_TYPE_INJECT_SCROLL_EVENT);
    view.setInt32(1, x);
    view.setInt32(5, y);
    view.setUint16(9, screenW);
    view.setUint16(11, screenH);
    // Normalize to [-1, 1] range and convert to i16 fixed-point
    const hNorm = Math.max(-1, Math.min(1, hscroll / 16));
    const vNorm = Math.max(-1, Math.min(1, vscroll / 16));
    view.setInt16(13, floatToI16fp(hNorm));
    view.setInt16(15, floatToI16fp(vNorm));
    view.setUint32(17, buttons || 0);
    return new Uint8Array(buf);
}

/**
 * BACK_OR_SCREEN_ON: 2 bytes
 */
function serializeBackOrScreenOn(action) {
    return new Uint8Array([MSG_TYPE_BACK_OR_SCREEN_ON, action]);
}

/**
 * Simple 1-byte messages
 */
function serializeExpandNotificationPanel() {
    return new Uint8Array([MSG_TYPE_EXPAND_NOTIFICATION_PANEL]);
}

function serializeExpandSettingsPanel() {
    return new Uint8Array([MSG_TYPE_EXPAND_SETTINGS_PANEL]);
}

function serializeCollapsePanels() {
    return new Uint8Array([MSG_TYPE_COLLAPSE_PANELS]);
}

function serializeRotateDevice() {
    return new Uint8Array([MSG_TYPE_ROTATE_DEVICE]);
}

function serializeSetDisplayPower(on) {
    return new Uint8Array([MSG_TYPE_SET_DISPLAY_POWER, on ? 1 : 0]);
}

export {
    MSG_TYPE_INJECT_KEYCODE, MSG_TYPE_INJECT_TEXT,
    MSG_TYPE_INJECT_TOUCH_EVENT, MSG_TYPE_INJECT_SCROLL_EVENT,
    AKEY_ACTION_DOWN, AKEY_ACTION_UP,
    AMOTION_ACTION_DOWN, AMOTION_ACTION_UP, AMOTION_ACTION_MOVE,
    POINTER_ID_MOUSE, POINTER_ID_FINGER,
    serializeInjectKeycode, serializeInjectTouch, serializeInjectScroll,
    serializeBackOrScreenOn,
    serializeExpandNotificationPanel, serializeExpandSettingsPanel,
    serializeCollapsePanels, serializeRotateDevice, serializeSetDisplayPower,
};
