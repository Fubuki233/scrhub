/**
 * Minimal fMP4 (fragmented MP4) muxer for H.264 video.
 * Converts H.264 Annex B NAL units into fMP4 init + media segments
 * suitable for Media Source Extensions (MSE).
 */

// Box helper: creates an MP4 box with type and payload
function box(type, ...payloads) {
    let size = 8;
    for (const p of payloads) size += p.byteLength;
    const result = new Uint8Array(size);
    const view = new DataView(result.buffer);
    view.setUint32(0, size);
    result[4] = type.charCodeAt(0);
    result[5] = type.charCodeAt(1);
    result[6] = type.charCodeAt(2);
    result[7] = type.charCodeAt(3);
    let offset = 8;
    for (const p of payloads) {
        result.set(p instanceof Uint8Array ? p : new Uint8Array(p), offset);
        offset += p.byteLength;
    }
    return result;
}

// Full box: version(1) + flags(3) + data
function fullBox(type, version, flags, ...payloads) {
    const header = new Uint8Array(4);
    header[0] = version;
    header[1] = (flags >> 16) & 0xFF;
    header[2] = (flags >> 8) & 0xFF;
    header[3] = flags & 0xFF;
    return box(type, header, ...payloads);
}

function u32(v) {
    const b = new Uint8Array(4);
    new DataView(b.buffer).setUint32(0, v);
    return b;
}

function u16(v) {
    const b = new Uint8Array(2);
    new DataView(b.buffer).setUint16(0, v);
    return b;
}

/**
 * Parse Annex B byte stream into individual NAL units.
 */
function parseAnnexBNALUs(data) {
    const nalus = [];
    const len = data.length;
    let i = 0;
    while (i < len) {
        if (i + 2 < len && data[i] === 0 && data[i + 1] === 0) {
            let scLen = 0;
            if (data[i + 2] === 1) {
                scLen = 3;
            } else if (i + 3 < len && data[i + 2] === 0 && data[i + 3] === 1) {
                scLen = 4;
            }
            if (scLen > 0) {
                const naluStart = i + scLen;
                let naluEnd = len;
                for (let j = naluStart + 1; j + 2 < len; j++) {
                    if (data[j] === 0 && data[j + 1] === 0 &&
                        (data[j + 2] === 1 || (j + 3 < len && data[j + 2] === 0 && data[j + 3] === 1))) {
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
 * Build an fMP4 init segment (ftyp + moov) for H.264 video.
 * @param {Uint8Array} sps - SPS NAL unit (without start code)
 * @param {Uint8Array} pps - PPS NAL unit (without start code)
 * @param {number} width - video width
 * @param {number} height - video height
 * @param {number} timescale - timescale (default 90000)
 */
function createInitSegment(sps, pps, width, height, timescale = 90000) {
    // ftyp
    const ftyp = box('ftyp',
        new Uint8Array([0x69, 0x73, 0x6F, 0x6D]),   // major_brand: isom
        u32(0x200),                                    // minor_version
        new Uint8Array([0x69, 0x73, 0x6F, 0x6D]),   // isom
        new Uint8Array([0x69, 0x73, 0x6F, 0x32]),   // iso2
        new Uint8Array([0x61, 0x76, 0x63, 0x31]),   // avc1
        new Uint8Array([0x6D, 0x70, 0x34, 0x31]),   // mp41
    );

    // avcC (AVCDecoderConfigurationRecord)
    const avcCData = new Uint8Array(11 + sps.length + pps.length);
    let off = 0;
    avcCData[off++] = 1;           // configurationVersion
    avcCData[off++] = sps[1];     // profile
    avcCData[off++] = sps[2];     // profile_compatibility
    avcCData[off++] = sps[3];     // level
    avcCData[off++] = 0xFF;       // lengthSizeMinusOne = 3
    avcCData[off++] = 0xE1;       // numSPS = 1
    avcCData[off++] = (sps.length >> 8) & 0xFF;
    avcCData[off++] = sps.length & 0xFF;
    avcCData.set(sps, off); off += sps.length;
    avcCData[off++] = 1;           // numPPS
    avcCData[off++] = (pps.length >> 8) & 0xFF;
    avcCData[off++] = pps.length & 0xFF;
    avcCData.set(pps, off);

    const avcC = box('avcC', avcCData);

    // avc1 (Visual Sample Entry)
    const avc1Data = new Uint8Array(78);
    const avc1View = new DataView(avc1Data.buffer);
    // 6 bytes reserved + 2 bytes data_reference_index
    avc1View.setUint16(6, 1);     // data_reference_index
    avc1View.setUint16(24, width);
    avc1View.setUint16(26, height);
    avc1View.setUint32(28, 0x00480000); // horizontal_resolution 72dpi
    avc1View.setUint32(32, 0x00480000); // vertical_resolution 72dpi
    avc1View.setUint16(40, 1);   // frame_count
    // 32 bytes compressor_name (all zeros is fine)
    avc1View.setUint16(74, 0x0018); // depth = 24
    avc1View.setInt16(76, -1);    // pre_defined = -1

    const avc1 = box('avc1', avc1Data, avcC);

    // stsd
    const stsd = fullBox('stsd', 0, 0, u32(1), avc1); // entry_count = 1

    // stts, stsc, stsz, stco (all empty for fragmented MP4)
    const stts = fullBox('stts', 0, 0, u32(0));
    const stsc = fullBox('stsc', 0, 0, u32(0));
    const stsz = fullBox('stsz', 0, 0, u32(0), u32(0));
    const stco = fullBox('stco', 0, 0, u32(0));

    const stbl = box('stbl', stsd, stts, stsc, stsz, stco);

    // dinf/dref
    const dref = fullBox('dref', 0, 0, u32(1),
        fullBox('url ', 0, 1) // self-contained
    );
    const dinf = box('dinf', dref);

    // vmhd
    const vmhd = fullBox('vmhd', 0, 1, new Uint8Array(8)); // graphicsmode + opcolor

    const minf = box('minf', vmhd, dinf, stbl);

    // hdlr
    const hdlrData = new Uint8Array(21);
    // handler_type = 'vide'
    hdlrData[4] = 0x76; hdlrData[5] = 0x69; hdlrData[6] = 0x64; hdlrData[7] = 0x65;
    // name = "VideoHandler\0"
    const handlerName = new TextEncoder().encode('VideoHandler\0');
    const hdlr = fullBox('hdlr', 0, 0, new Uint8Array(hdlrData.buffer, 0, 8), handlerName);

    // mdhd
    const mdhdData = new Uint8Array(20);
    const mdhdView = new DataView(mdhdData.buffer);
    mdhdView.setUint32(8, timescale); // timescale
    mdhdView.setUint16(16, 0x55C4);  // language: und
    const mdhd = fullBox('mdhd', 0, 0, mdhdData);

    const mdia = box('mdia', mdhd, hdlr, minf);

    // tkhd
    const tkhdData = new Uint8Array(80);
    const tkhdView = new DataView(tkhdData.buffer);
    tkhdView.setUint32(0, 0);       // creation_time
    tkhdView.setUint32(4, 0);       // modification_time
    tkhdView.setUint32(8, 1);       // track_ID
    // 4 bytes reserved
    tkhdView.setUint32(16, 0);      // duration (unknown for live)
    // 8 bytes reserved
    tkhdView.setInt16(28, 0);       // layer
    tkhdView.setInt16(30, 0);       // alternate_group
    tkhdView.setInt16(32, 0);       // volume (0 for video)
    // 2 bytes reserved
    // identity matrix (36..71)
    tkhdView.setUint32(36, 0x00010000);
    tkhdView.setUint32(52, 0x00010000);
    tkhdView.setUint32(68, 0x40000000);
    tkhdView.setUint32(72, width << 16);   // width (fixed-point)
    tkhdView.setUint32(76, height << 16);  // height (fixed-point)
    const tkhd = fullBox('tkhd', 0, 3, tkhdData); // flags = track_enabled | track_in_movie

    const trak = box('trak', tkhd, mdia);

    // trex
    const trexData = new Uint8Array(20);
    const trexView = new DataView(trexData.buffer);
    trexView.setUint32(0, 1);  // track_ID
    trexView.setUint32(4, 1);  // default_sample_description_index
    trexView.setUint32(8, 0);  // default_sample_duration
    trexView.setUint32(12, 0); // default_sample_size
    trexView.setUint32(16, 0); // default_sample_flags
    const trex = fullBox('trex', 0, 0, trexData);
    const mvex = box('mvex', trex);

    // mvhd
    const mvhdData = new Uint8Array(96);
    const mvhdView = new DataView(mvhdData.buffer);
    mvhdView.setUint32(8, timescale); // timescale
    mvhdView.setUint32(12, 0);       // duration
    mvhdView.setUint32(16, 0x00010000); // rate = 1.0
    mvhdView.setUint16(20, 0x0100);    // volume = 1.0
    // identity matrix
    mvhdView.setUint32(32, 0x00010000);
    mvhdView.setUint32(48, 0x00010000);
    mvhdView.setUint32(64, 0x40000000);
    mvhdView.setUint32(92, 2);        // next_track_ID
    const mvhd = fullBox('mvhd', 0, 0, mvhdData);

    const moov = box('moov', mvhd, trak, mvex);

    // Concatenate ftyp + moov
    const init = new Uint8Array(ftyp.length + moov.length);
    init.set(ftyp);
    init.set(moov, ftyp.length);
    return init;
}

/**
 * Create a media segment (moof + mdat) containing one or more samples.
 * @param {number} sequenceNumber - moof sequence counter
 * @param {number} baseDecodeTime - DTS in timescale units
 * @param {Array<{data: Uint8Array, duration: number, isKey: boolean}>} samples
 */
function createMediaSegment(sequenceNumber, baseDecodeTime, samples) {
    // trun flags: data-offset-present | sample-duration-present | sample-size-present | sample-flags-present
    const trunFlags = 0x000301;  // data-offset(1) + sample-duration(0x100) + sample-size(0x200)
    // Actually let's use more flags
    const TRUN_DATA_OFFSET = 0x01;
    const TRUN_FIRST_SAMPLE_FLAGS = 0x04;
    const TRUN_SAMPLE_DURATION = 0x100;
    const TRUN_SAMPLE_SIZE = 0x200;
    const TRUN_SAMPLE_FLAGS = 0x400;

    const flags = TRUN_DATA_OFFSET | TRUN_SAMPLE_DURATION | TRUN_SAMPLE_SIZE | TRUN_SAMPLE_FLAGS;

    // Calculate total data size
    let totalDataSize = 0;
    for (const s of samples) totalDataSize += s.data.length;

    // Build trun
    const trunEntrySize = 12; // duration(4) + size(4) + flags(4)
    const trunDataLen = 4 + 4 + samples.length * trunEntrySize; // sample_count + data_offset + entries
    const trunData = new Uint8Array(trunDataLen);
    const trunView = new DataView(trunData.buffer);
    trunView.setUint32(0, samples.length); // sample_count
    // data_offset will be filled after we know moof size
    let trunOff = 8; // skip sample_count(4) + data_offset(4)
    for (const s of samples) {
        trunView.setUint32(trunOff, s.duration); trunOff += 4;
        trunView.setUint32(trunOff, s.data.length); trunOff += 4;
        // sample_flags: key frame = 0x02000000, non-key = 0x01010000
        const sf = s.isKey ? 0x02000000 : 0x01010000;
        trunView.setUint32(trunOff, sf); trunOff += 4;
    }
    const trun = fullBox('trun', 0, flags, trunData);

    // tfdt
    const tfdtData = new Uint8Array(8);
    new DataView(tfdtData.buffer).setUint32(4, baseDecodeTime & 0xFFFFFFFF);
    // Use version 1 for 64-bit base_decode_time
    const tfdtDataV1 = new Uint8Array(8);
    const tfdtV1View = new DataView(tfdtDataV1.buffer);
    tfdtV1View.setUint32(0, (baseDecodeTime / 0x100000000) >>> 0);
    tfdtV1View.setUint32(4, baseDecodeTime & 0xFFFFFFFF);
    const tfdt = fullBox('tfdt', 1, 0, tfdtDataV1);

    // tfhd
    const tfhdData = new Uint8Array(4);
    new DataView(tfhdData.buffer).setUint32(0, 1); // track_ID
    const tfhd = fullBox('tfhd', 0, 0x020000, tfhdData); // default-base-is-moof

    const traf = box('traf', tfhd, tfdt, trun);

    // mfhd
    const mfhdData = new Uint8Array(4);
    new DataView(mfhdData.buffer).setUint32(0, sequenceNumber);
    const mfhd = fullBox('mfhd', 0, 0, mfhdData);

    const moof = box('moof', mfhd, traf);

    // Fix data_offset in trun: offset from start of moof to start of mdat payload
    const moofSize = moof.length;
    const mdatHeaderSize = 8;
    const dataOffset = moofSize + mdatHeaderSize;

    // Find trun data_offset position within moof
    // trun is inside traf which is inside moof
    // data_offset is at: trun_box_start + 8(box header) + 4(fullbox header) + 4(sample_count) = +16
    // We need to find trun within the assembled moof
    const trunMarker = findBoxOffset(moof, 'trun');
    if (trunMarker >= 0) {
        const dv = new DataView(moof.buffer, moof.byteOffset);
        dv.setUint32(trunMarker + 12 + 4, dataOffset); // +12 for box+fullbox header, +4 for sample_count
    }

    // mdat
    const mdat = new Uint8Array(8 + totalDataSize);
    const mdatView = new DataView(mdat.buffer);
    mdatView.setUint32(0, 8 + totalDataSize);
    mdat[4] = 0x6D; mdat[5] = 0x64; mdat[6] = 0x61; mdat[7] = 0x74; // 'mdat'
    let mdatOff = 8;
    for (const s of samples) {
        mdat.set(s.data, mdatOff);
        mdatOff += s.data.length;
    }

    const segment = new Uint8Array(moof.length + mdat.length);
    segment.set(moof);
    segment.set(mdat, moof.length);
    return segment;
}

function findBoxOffset(data, type) {
    const t0 = type.charCodeAt(0), t1 = type.charCodeAt(1), t2 = type.charCodeAt(2), t3 = type.charCodeAt(3);
    for (let i = 0; i + 7 < data.length; i++) {
        if (data[i + 4] === t0 && data[i + 5] === t1 && data[i + 6] === t2 && data[i + 7] === t3) {
            return i;
        }
    }
    return -1;
}

/**
 * Convert Annex B frame data to AVCC format (4-byte length prefix per NALU).
 * Filters out SPS/PPS NALUs from regular frames.
 */
function annexBToAvcc(data, filterParamSets = true) {
    const nalus = parseAnnexBNALUs(data);
    const filtered = filterParamSets
        ? nalus.filter(n => { const t = n[0] & 0x1F; return t !== 7 && t !== 8; })
        : nalus;
    let totalLen = 0;
    for (const nalu of filtered) totalLen += 4 + nalu.length;
    const result = new Uint8Array(totalLen);
    let off = 0;
    for (const nalu of filtered) {
        result[off++] = (nalu.length >> 24) & 0xFF;
        result[off++] = (nalu.length >> 16) & 0xFF;
        result[off++] = (nalu.length >> 8) & 0xFF;
        result[off++] = nalu.length & 0xFF;
        result.set(nalu, off);
        off += nalu.length;
    }
    return result;
}

export { parseAnnexBNALUs, createInitSegment, createMediaSegment, annexBToAvcc };
