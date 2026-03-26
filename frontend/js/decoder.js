/**
 * Video decoder using WebCodecs API for web-scrcpy.
 * Receives raw scrcpy stream packets (12-byte header + Annex B encoded data).
 * Converts Annex B → AVCC format for WebCodecs compatibility.
 */

// Scrcpy packet constants
const PACKET_HEADER_SIZE = 12;
const FLAG_CONFIG = BigInt(1) << BigInt(63);
const FLAG_KEY_FRAME = BigInt(1) << BigInt(62);
const PTS_MASK = FLAG_KEY_FRAME - BigInt(1);

// H.264 NAL unit types
const NAL_SPS = 7;
const NAL_PPS = 8;

/**
 * Parse Annex B byte stream into individual NAL units.
 * Handles both 3-byte (00 00 01) and 4-byte (00 00 00 01) start codes.
 */
function parseAnnexBNALUs(data) {
    const nalus = [];
    const len = data.length;
    let i = 0;

    while (i < len) {
        // Find start code
        if (i + 2 < len && data[i] === 0 && data[i + 1] === 0) {
            let scLen = 0;
            if (data[i + 2] === 1) {
                scLen = 3;
            } else if (i + 3 < len && data[i + 2] === 0 && data[i + 3] === 1) {
                scLen = 4;
            }
            if (scLen > 0) {
                const naluStart = i + scLen;
                // Find end: next start code or end of data
                let naluEnd = len;
                for (let j = naluStart + 1; j + 2 < len; j++) {
                    if (data[j] === 0 && data[j + 1] === 0 &&
                        (data[j + 2] === 1 || (j + 3 < len && data[j + 2] === 0 && data[j + 3] === 1))) {
                        // Remove trailing zeros that belong to the next start code
                        naluEnd = j;
                        break;
                    }
                }
                if (naluStart < naluEnd) {
                    nalus.push(data.subarray(naluStart, naluEnd));
                }
                i = naluEnd;
                continue;
            }
        }
        i++;
    }
    return nalus;
}

/**
 * Build AVCDecoderConfigurationRecord from SPS and PPS NAL units.
 * Format per ISO 14496-15.
 */
function buildAvcC(sps, pps) {
    // avcC: version(1) + profile(1) + compat(1) + level(1) + lengthSizeMinusOne(1)
    //       + numSPS(1) + spsLen(2) + spsData + numPPS(1) + ppsLen(2) + ppsData
    const size = 6 + 2 + sps.length + 1 + 2 + pps.length;
    const buf = new Uint8Array(size);
    let off = 0;
    buf[off++] = 1;               // configurationVersion
    buf[off++] = sps[1];          // AVCProfileIndication
    buf[off++] = sps[2];          // profile_compatibility
    buf[off++] = sps[3];          // AVCLevelIndication
    buf[off++] = 0xFF;            // lengthSizeMinusOne = 3 (4-byte NALU lengths)
    buf[off++] = 0xE1;            // numOfSequenceParameterSets = 1
    buf[off++] = (sps.length >> 8) & 0xFF;
    buf[off++] = sps.length & 0xFF;
    buf.set(sps, off); off += sps.length;
    buf[off++] = 1;               // numOfPictureParameterSets
    buf[off++] = (pps.length >> 8) & 0xFF;
    buf[off++] = pps.length & 0xFF;
    buf.set(pps, off);
    return buf;
}

/**
 * Convert Annex B frame data to AVCC format (4-byte length prefix per NALU).
 */
function annexBToAvcc(data) {
    const nalus = parseAnnexBNALUs(data);
    let totalLen = 0;
    for (const nalu of nalus) totalLen += 4 + nalu.length;
    const result = new Uint8Array(totalLen);
    let off = 0;
    for (const nalu of nalus) {
        result[off++] = (nalu.length >> 24) & 0xFF;
        result[off++] = (nalu.length >> 16) & 0xFF;
        result[off++] = (nalu.length >> 8) & 0xFF;
        result[off++] = nalu.length & 0xFF;
        result.set(nalu, off);
        off += nalu.length;
    }
    return result;
}

class VideoDecoder2 {
    constructor(canvas) {
        this.canvas = canvas;
        this.ctx = canvas.getContext('2d');
        this.decoder = null;
        this.codecString = null;
        this.configData = null;
        this.isH264 = false;
        this.width = 0;
        this.height = 0;
        this.frameCount = 0;
        this.ready = false;

        // When true, drop all frames until the next keyframe (after a reset)
        this._waitingForKeyframe = false;
    }

    init(codec, width, height) {
        this.codecString = codec;
        this.isH264 = codec.startsWith('avc');
        this.width = width;
        this.height = height;
        this.canvas.width = width;
        this.canvas.height = height;

        if (typeof window.VideoDecoder === 'undefined') {
            console.warn('WebCodecs VideoDecoder not available');
            return;
        }

        this.decoder = new window.VideoDecoder({
            output: (frame) => this._onFrame(frame),
            error: (e) => console.error('VideoDecoder error:', e),
        });

        this.ready = false;
        console.log(`[decoder] init codec=${codec} ${width}x${height}`);
    }

    feed(packet) {
        if (!this.decoder || packet.length < PACKET_HEADER_SIZE) return;

        const view = new DataView(packet.buffer, packet.byteOffset, packet.byteLength);
        const ptsHi = view.getUint32(0);
        const ptsLo = view.getUint32(4);
        const ptsFlags = (BigInt(ptsHi) << BigInt(32)) | BigInt(ptsLo);
        const payloadSize = view.getUint32(8);

        const isConfig = (ptsFlags & FLAG_CONFIG) !== BigInt(0);
        const isKeyFrame = (ptsFlags & FLAG_KEY_FRAME) !== BigInt(0);
        const pts = ptsFlags & PTS_MASK;

        const payload = packet.subarray(PACKET_HEADER_SIZE, PACKET_HEADER_SIZE + payloadSize);

        if (isConfig) {
            if (this.isH264) {
                // Parse Annex B config → extract SPS/PPS → build avcC record
                const nalus = parseAnnexBNALUs(payload);
                let sps = null, pps = null;
                for (const nalu of nalus) {
                    const naluType = nalu[0] & 0x1F;
                    if (naluType === NAL_SPS && !sps) sps = nalu;
                    if (naluType === NAL_PPS && !pps) pps = nalu;
                }
                if (sps && pps) {
                    this.configData = buildAvcC(sps, pps);
                    // Derive accurate codec string from SPS profile/level
                    const profile = sps[1], compat = sps[2], level = sps[3];
                    this.codecString = 'avc1.' +
                        profile.toString(16).padStart(2, '0') +
                        compat.toString(16).padStart(2, '0') +
                        level.toString(16).padStart(2, '0');
                    console.log(`[decoder] SPS/PPS parsed, codec=${this.codecString}`);
                } else {
                    console.warn('[decoder] config packet missing SPS or PPS');
                    this.configData = new Uint8Array(payload);
                }
            } else {
                // Non-H.264: pass config data as-is
                this.configData = new Uint8Array(payload);
            }
            this._configure();
            return;
        }

        if (this.decoder.state !== 'configured') return;

        // If decoder queue is severely backed up, reset and wait for keyframe.
        // This avoids accumulating latency. We reset (discard everything in
        // the queue) then reconfigure, so the next keyframe starts clean.
        if (this.decoder.decodeQueueSize > 10) {
            console.warn(`[decoder] queue backed up (${this.decoder.decodeQueueSize}), resetting`);
            this.decoder.reset();
            this._configure();
            this._waitingForKeyframe = true;
            return;
        }

        // After a reset, drop everything until we get a clean keyframe
        if (this._waitingForKeyframe) {
            if (!isKeyFrame) return;
            this._waitingForKeyframe = false;
        }

        // Convert Annex B → AVCC for H.264 frames
        const frameData = this.isH264 ? annexBToAvcc(payload) : payload;

        const chunk = new EncodedVideoChunk({
            type: isKeyFrame ? 'key' : 'delta',
            timestamp: Number(pts),
            data: frameData,
        });

        try {
            this.decoder.decode(chunk);
        } catch (e) {
            console.warn('decode error:', e);
        }
    }

    _configure() {
        if (!this.decoder || !this.codecString) return;

        const config = {
            codec: this.codecString,
            codedWidth: this.width,
            codedHeight: this.height,
            optimizeForLatency: true,
            hardwareAcceleration: 'prefer-hardware',
        };

        if (this.configData) {
            config.description = this.configData;
        }

        try {
            this.decoder.configure(config);
            this.ready = true;
            console.log(`[decoder] configured: ${this.codecString} ${this.width}x${this.height}`);
        } catch (e) {
            console.error('Failed to configure VideoDecoder:', e);
        }
    }

    _onFrame(frame) {
        // Render immediately for minimum latency — no rAF delay
        if (frame.displayWidth !== this.canvas.width || frame.displayHeight !== this.canvas.height) {
            this.canvas.width = frame.displayWidth;
            this.canvas.height = frame.displayHeight;
            this.width = frame.displayWidth;
            this.height = frame.displayHeight;
        }
        this.ctx.drawImage(frame, 0, 0);
        frame.close();
        this.frameCount++;
    }

    destroy() {
        if (this.decoder && this.decoder.state !== 'closed') {
            this.decoder.close();
        }
        this.decoder = null;
    }
}

export { VideoDecoder2 };
