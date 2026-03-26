/**
 * Input handler for web-scrcpy.
 * Captures mouse, touch, keyboard, and scroll events on the canvas
 * and converts them to scrcpy control messages.
 */

import {
    AKEY_ACTION_DOWN, AKEY_ACTION_UP,
    AMOTION_ACTION_DOWN, AMOTION_ACTION_UP, AMOTION_ACTION_MOVE,
    POINTER_ID_MOUSE, POINTER_ID_FINGER,
    serializeInjectKeycode, serializeInjectTouch, serializeInjectScroll,
    serializeBackOrScreenOn,
} from './protocol.js';

// Keyboard: browser key code → Android AKEYCODE mapping (common keys)
const KEY_MAP = {
    'Backspace': 67, 'Tab': 61, 'Enter': 66, 'Escape': 111,
    'Space': 62, 'Delete': 112,
    'ArrowLeft': 21, 'ArrowUp': 19, 'ArrowRight': 22, 'ArrowDown': 20,
    'Home': 3, 'End': 123, 'PageUp': 92, 'PageDown': 93,
    'KeyA': 29, 'KeyB': 30, 'KeyC': 31, 'KeyD': 32, 'KeyE': 33,
    'KeyF': 34, 'KeyG': 35, 'KeyH': 36, 'KeyI': 37, 'KeyJ': 38,
    'KeyK': 39, 'KeyL': 40, 'KeyM': 41, 'KeyN': 42, 'KeyO': 43,
    'KeyP': 44, 'KeyQ': 45, 'KeyR': 46, 'KeyS': 47, 'KeyT': 48,
    'KeyU': 49, 'KeyV': 50, 'KeyW': 51, 'KeyX': 52, 'KeyY': 53,
    'KeyZ': 54,
    'Digit0': 7, 'Digit1': 8, 'Digit2': 9, 'Digit3': 10,
    'Digit4': 11, 'Digit5': 12, 'Digit6': 13, 'Digit7': 14,
    'Digit8': 15, 'Digit9': 16,
    'Minus': 69, 'Equal': 70, 'BracketLeft': 71, 'BracketRight': 72,
    'Backslash': 73, 'Semicolon': 74, 'Quote': 75, 'Comma': 55,
    'Period': 56, 'Slash': 76, 'Backquote': 68,
    'F1': 131, 'F2': 132, 'F3': 133, 'F4': 134, 'F5': 135,
    'F6': 136, 'F7': 137, 'F8': 138, 'F9': 139, 'F10': 140,
    'F11': 141, 'F12': 142,
    'VolumeUp': 24, 'VolumeDown': 25,
};

// Android meta state flags
const META_SHIFT = 0x01;
const META_CTRL = 0x1000;
const META_ALT = 0x02;

class InputHandler {
    /**
     * @param {HTMLCanvasElement} canvas
     * @param {function(Uint8Array): void} sendControl - sends binary control message
     * @param {function(): {width: number, height: number}} getDeviceSize
     */
    constructor(canvas, sendControl, getDeviceSize) {
        this.canvas = canvas;
        this.sendControl = sendControl;
        this.getDeviceSize = getDeviceSize;
        this.enabled = false;

        // Mouse state
        this.mouseDown = false;
        this.mouseButtons = 0;

        // Touch tracking
        this.activeTouches = new Map(); // touch identifier → pointer_id (bigint)
        this.nextTouchId = 0;

        this._boundHandlers = {};
    }

    enable() {
        if (this.enabled) return;
        this.enabled = true;

        const c = this.canvas;

        // Mouse events
        this._bind(c, 'mousedown', this._onMouseDown);
        this._bind(c, 'mouseup', this._onMouseUp);
        this._bind(c, 'mousemove', this._onMouseMove);
        this._bind(c, 'wheel', this._onWheel);
        this._bind(c, 'contextmenu', (e) => e.preventDefault());

        // Touch events
        this._bind(c, 'touchstart', this._onTouchStart);
        this._bind(c, 'touchmove', this._onTouchMove);
        this._bind(c, 'touchend', this._onTouchEnd);
        this._bind(c, 'touchcancel', this._onTouchEnd);

        // Keyboard events (on document to capture regardless of focus)
        this._bind(document, 'keydown', this._onKeyDown);
        this._bind(document, 'keyup', this._onKeyUp);
    }

    disable() {
        if (!this.enabled) return;
        this.enabled = false;

        for (const [target, event, handler] of this._boundHandlers.list || []) {
            target.removeEventListener(event, handler);
        }
        this._boundHandlers = {};
    }

    _bind(target, event, handler) {
        const bound = handler.bind(this);
        target.addEventListener(event, bound, { passive: false });
        if (!this._boundHandlers.list) this._boundHandlers.list = [];
        this._boundHandlers.list.push([target, event, bound]);
    }

    /**
     * Convert canvas pixel coordinates to device coordinates.
     */
    _toDeviceCoords(clientX, clientY) {
        const rect = this.canvas.getBoundingClientRect();
        const { width: devW, height: devH } = this.getDeviceSize();

        // Canvas displays the device screen, potentially with aspect ratio preservation
        const scaleX = devW / rect.width;
        const scaleY = devH / rect.height;

        const x = Math.round((clientX - rect.left) * scaleX);
        const y = Math.round((clientY - rect.top) * scaleY);

        return {
            x: Math.max(0, Math.min(devW - 1, x)),
            y: Math.max(0, Math.min(devH - 1, y)),
            screenW: devW,
            screenH: devH,
        };
    }

    _getMetaState(e) {
        let meta = 0;
        if (e.shiftKey) meta |= META_SHIFT;
        if (e.ctrlKey) meta |= META_CTRL;
        if (e.altKey) meta |= META_ALT;
        return meta;
    }

    _androidButtons(e) {
        let buttons = 0;
        if (e.buttons & 1) buttons |= 1;  // PRIMARY
        if (e.buttons & 2) buttons |= 2;  // SECONDARY
        if (e.buttons & 4) buttons |= 4;  // TERTIARY
        return buttons;
    }

    // Mouse handlers
    _onMouseDown(e) {
        e.preventDefault();
        this.mouseDown = true;
        this.mouseButtons = this._androidButtons(e);
        const pos = this._toDeviceCoords(e.clientX, e.clientY);
        const actionButton = 1 << (e.button); // BUTTON_PRIMARY=1, SECONDARY=2, TERTIARY=4
        const msg = serializeInjectTouch(
            AMOTION_ACTION_DOWN, POINTER_ID_MOUSE,
            pos.x, pos.y, pos.screenW, pos.screenH,
            1.0, actionButton, this.mouseButtons
        );
        this.sendControl(msg);
    }

    _onMouseUp(e) {
        e.preventDefault();
        this.mouseButtons = this._androidButtons(e);
        const pos = this._toDeviceCoords(e.clientX, e.clientY);
        const actionButton = 1 << (e.button);
        const msg = serializeInjectTouch(
            AMOTION_ACTION_UP, POINTER_ID_MOUSE,
            pos.x, pos.y, pos.screenW, pos.screenH,
            0.0, actionButton, this.mouseButtons
        );
        this.sendControl(msg);
        this.mouseDown = false;
    }

    _onMouseMove(e) {
        if (!this.mouseDown) return;
        e.preventDefault();
        this.mouseButtons = this._androidButtons(e);
        const pos = this._toDeviceCoords(e.clientX, e.clientY);
        const msg = serializeInjectTouch(
            AMOTION_ACTION_MOVE, POINTER_ID_MOUSE,
            pos.x, pos.y, pos.screenW, pos.screenH,
            1.0, 0, this.mouseButtons
        );
        this.sendControl(msg);
    }

    _onWheel(e) {
        e.preventDefault();
        const pos = this._toDeviceCoords(e.clientX, e.clientY);
        // deltaY: positive = scroll down, negative = scroll up
        // scrcpy convention: positive vscroll = scroll down
        const hscroll = -e.deltaX / Math.abs(e.deltaX || 1);
        const vscroll = -e.deltaY / Math.abs(e.deltaY || 1);
        const msg = serializeInjectScroll(
            pos.x, pos.y, pos.screenW, pos.screenH,
            hscroll, vscroll, this._androidButtons(e)
        );
        this.sendControl(msg);
    }

    // Touch handlers
    _onTouchStart(e) {
        e.preventDefault();
        for (const touch of e.changedTouches) {
            const pointerId = BigInt(this.nextTouchId++);
            this.activeTouches.set(touch.identifier, pointerId);
            const pos = this._toDeviceCoords(touch.clientX, touch.clientY);
            const msg = serializeInjectTouch(
                AMOTION_ACTION_DOWN, pointerId,
                pos.x, pos.y, pos.screenW, pos.screenH,
                touch.force || 1.0, 0, 0
            );
            this.sendControl(msg);
        }
    }

    _onTouchMove(e) {
        e.preventDefault();
        for (const touch of e.changedTouches) {
            const pointerId = this.activeTouches.get(touch.identifier);
            if (pointerId === undefined) continue;
            const pos = this._toDeviceCoords(touch.clientX, touch.clientY);
            const msg = serializeInjectTouch(
                AMOTION_ACTION_MOVE, pointerId,
                pos.x, pos.y, pos.screenW, pos.screenH,
                touch.force || 1.0, 0, 0
            );
            this.sendControl(msg);
        }
    }

    _onTouchEnd(e) {
        e.preventDefault();
        for (const touch of e.changedTouches) {
            const pointerId = this.activeTouches.get(touch.identifier);
            if (pointerId === undefined) continue;
            this.activeTouches.delete(touch.identifier);
            const pos = this._toDeviceCoords(touch.clientX, touch.clientY);
            const msg = serializeInjectTouch(
                AMOTION_ACTION_UP, pointerId,
                pos.x, pos.y, pos.screenW, pos.screenH,
                0.0, 0, 0
            );
            this.sendControl(msg);
        }
    }

    // Keyboard handlers
    _onKeyDown(e) {
        const keycode = KEY_MAP[e.code];
        if (keycode === undefined) return;
        e.preventDefault();
        const msg = serializeInjectKeycode(AKEY_ACTION_DOWN, keycode, 0, this._getMetaState(e));
        this.sendControl(msg);
    }

    _onKeyUp(e) {
        const keycode = KEY_MAP[e.code];
        if (keycode === undefined) return;
        e.preventDefault();
        const msg = serializeInjectKeycode(AKEY_ACTION_UP, keycode, 0, this._getMetaState(e));
        this.sendControl(msg);
    }
}

export { InputHandler };
