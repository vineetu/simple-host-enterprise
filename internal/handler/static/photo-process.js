/* Shared by /photos and the MCP photo picker. No network or account access. */
(function (root) {
  'use strict';
  const slug = name => name.toLowerCase().replace(/\.[^.]+$/, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60) || 'photo';
  const dataURL = blob => new Promise((resolve, reject) => {
    const reader = new FileReader(); reader.onload = () => resolve(reader.result); reader.onerror = reject; reader.readAsDataURL(blob);
  });
  function occupiedPaths(files) {
    const occupied = new Map();
    for (const file of files) {
      occupied.set(file.path, false);
      const parts = file.path.split('/');
      for (let i = 1; i < parts.length; i++) occupied.set(parts.slice(0, i).join('/'), true);
    }
    return occupied;
  }
  function photoPath(occupied, name) {
    if (occupied.get('images') === false) throw new Error('The images path is a file. Rename it before adding photos.');
    const dot = name.lastIndexOf('.'), stem = name.slice(0, dot), ext = name.slice(dot);
    let path = 'images/' + name;
    for (let n = 2; occupied.has(path); n++) {
      if (occupied.get(path)) throw new Error('A folder uses this photo name. Choose another name.');
      const suffix = '-' + n;
      path = 'images/' + stem.slice(0, 60 - suffix.length) + suffix + ext;
    }
    occupied.set(path, false);
    return path;
  }
  const digest = async blob => Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())), byte => byte.toString(16).padStart(2, '0')).join('');
  function deadline(work) {
    let timer;
    return Promise.race([work, new Promise((_, reject) => { timer = setTimeout(() => reject(unreadable()), 20000); })]).finally(() => clearTimeout(timer));
  }
  const unreadable = () => new Error('Could not read this photo');
  function checkDimensions(width, height) {
    if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height) || width < 1 || height < 1 || width * height > 50000000) throw unreadable();
  }
  // Inspect encoded headers before asking the browser to allocate pixels.
  // Unknown containers are refused rather than decoded without a size check.
  function nativeDimensions(buffer) {
    const b = new Uint8Array(buffer), v = new DataView(buffer);
    const text = (at, n) => String.fromCharCode(...b.subarray(at, at + n));
    const u16 = (at, le = false) => v.getUint16(at, le);
    const u32 = (at, le = false) => v.getUint32(at, le);
    const u24 = at => b[at] + b[at + 1] * 256 + b[at + 2] * 65536;
    let found = false;
    const dimensions = (w, h) => { checkDimensions(w, h); found = true; };
    if (text(0, 8) === '\x89PNG\r\n\x1a\n' && text(12, 4) === 'IHDR') {
      dimensions(u32(16), u32(20));
    } else if (b[0] === 255 && b[1] === 216) {
      for (let at = 2; at < b.length;) {
        if (b[at++] !== 255) throw unreadable();
        while (b[at] === 255) at++;
        const marker = b[at++];
        if (marker === 218 || marker === 217) break;
        const length = u16(at);
        if (length < 2 || at + length > b.length) throw unreadable();
        if ([192,193,194,195,197,198,199,201,202,203,205,206,207].includes(marker)) {
          dimensions(u16(at + 5), u16(at + 3));
        }
        at += length;
      }
    } else if (text(0, 4) === 'RIFF' && text(8, 4) === 'WEBP') {
      const chunks = (start, end) => {
        for (let at = start; at + 8 <= end;) {
          const type = text(at, 4), length = u32(at + 4, true), data = at + 8;
          if (data + length > end) throw unreadable();
          if (type === 'VP8X' && length >= 10) dimensions(u24(data + 4) + 1, u24(data + 7) + 1);
          if (type === 'VP8 ' && length >= 10) dimensions(u16(data + 6, true) & 16383, u16(data + 8, true) & 16383);
          if (type === 'VP8L' && length >= 5 && b[data] === 47) {
            const bits = u32(data + 1, true); dimensions((bits & 16383) + 1, ((bits >>> 14) & 16383) + 1);
          }
          if (type === 'ANMF') {
            if (length < 16 || start !== 12) throw unreadable();
            dimensions(u24(data + 6) + 1, u24(data + 9) + 1);
            chunks(data + 16, data + length);
          }
          at = data + length + (length % 2);
        }
      };
      chunks(12, b.length);
    } else if (text(0, 2) === 'BM') {
      const dib = u32(14, true);
      if (dib === 12) dimensions(u16(18, true), u16(20, true));
      else if (dib >= 40) dimensions(v.getInt32(18, true), Math.abs(v.getInt32(22, true)));
    } else if (text(0, 6) === 'GIF87a' || text(0, 6) === 'GIF89a') {
      dimensions(u16(6, true), u16(8, true));
      let at = 13 + ((b[10] & 128) ? 3 * (2 ** ((b[10] & 7) + 1)) : 0);
      const skipBlocks = () => { while (at < b.length) { const n = b[at++]; if (!n) return; at += n; } throw unreadable(); };
      while (at < b.length) {
        const kind = b[at++];
        if (kind === 59) break;
        if (kind === 33) { at++; skipBlocks(); }
        else if (kind === 44) {
          dimensions(u16(at + 4, true), u16(at + 6, true));
          const flags = b[at + 8]; at += 9 + ((flags & 128) ? 3 * (2 ** ((flags & 7) + 1)) : 0);
          at++; skipBlocks();
        } else throw unreadable();
      }
    } else if (text(4, 4) === 'ftyp' && /avif|avis/.test(text(8, Math.min(u32(0) - 8, 256)))) {
      // AVIF item properties contain spatial extents, including derived grids.
      const boxes = (start, end, depth) => {
        if (depth > 4) throw unreadable();
        for (let at = start; at + 8 <= end;) {
          let length = u32(at), header = 8;
          if (length === 1) { if (u32(at + 8) !== 0) throw unreadable(); length = u32(at + 12); header = 16; }
          if (length === 0) length = end - at;
          if (length < header || at + length > end) throw unreadable();
          const type = text(at + 4, 4), data = at + header;
          if (type === 'ispe' && length >= header + 12) dimensions(u32(data + 4), u32(data + 8));
          if (['meta', 'iprp', 'ipco'].includes(type)) boxes(data + (type === 'meta' ? 4 : 0), at + length, depth + 1);
          at += length;
        }
      };
      boxes(0, b.length, 0);
    }
    if (!found) throw unreadable();
  }
  // The timeout includes file reads and fallback loads. A late bitmap is closed.
  async function nativeDecode(file) {
    let expired = false, img, reader, timer;
    const work = async () => {
      nativeDimensions(await file.arrayBuffer());
      if (expired) throw unreadable();
      if (typeof createImageBitmap === 'function') {
        let bitmap;
        try { bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' }); }
        catch (error) { if (expired) throw error; }
        if (bitmap) {
          try {
            if (expired) throw unreadable();
            checkDimensions(bitmap.width, bitmap.height);
          } catch (error) { bitmap.close(); throw error; }
          return { source: bitmap, width: bitmap.width, height: bitmap.height, orientation: 1, close: () => bitmap.close() };
        }
      }
      const url = await new Promise((resolve, reject) => {
        reader = new FileReader(); reader.onload = () => resolve(reader.result); reader.onerror = reject; reader.onabort = reject; reader.readAsDataURL(file);
      });
      if (expired) throw unreadable();
      img = new Image();
      await new Promise((resolve, reject) => { img.onload = resolve; img.onerror = reject; img.src = url; });
      checkDimensions(img.naturalWidth, img.naturalHeight);
      return { source: img, width: img.naturalWidth, height: img.naturalHeight, orientation: 1, close: () => { img.src = ''; } };
    };
    try {
      return await Promise.race([work(), new Promise((_, reject) => { timer = setTimeout(() => { expired = true; reject(unreadable()); }, 20000); })]);
    } catch (_) {
      expired = true;
      if (reader?.readyState === 1) reader.abort();
      if (img) { img.onload = img.onerror = null; img.src = ''; }
      throw unreadable();
    } finally { clearTimeout(timer); }
  }
  async function codecKind(file) {
    const head = new Uint8Array(await deadline(file.slice(0, 256).arrayBuffer()));
    const ascii = String.fromCharCode(...head);
    if (/\.(heic|heif)$/i.test(file.name) || /image\/(heic|heif)/i.test(file.type) || (ascii.slice(4, 8) === 'ftyp' && /heic|heix|hevc|heif|mif1|msf1/.test(ascii.slice(8)))) return 'heif';
    if (/\.tiff?$/i.test(file.name) || /tiff/i.test(file.type) || ascii.startsWith('II*\0') || ascii.startsWith('MM\0*')) return 'tiff';
    return '';
  }
  async function decode(file, options = {}) {
    if (file.size > 80000000) throw unreadable();
    const kind = await codecKind(file);
    if (!kind) return nativeDecode(file);
    let worker, timeout, expired = false;
    try {
      const decoded = await Promise.race([(async () => {
        const buffer = await file.arrayBuffer();
        if (expired) throw unreadable();
        worker = options.workerFactory ? options.workerFactory() : new Worker('/photo-codecs/v3/decoder.js');
        return new Promise((resolve, reject) => {
          worker.onmessage = event => event.data.error ? reject(new Error(event.data.error)) : resolve(event.data);
          worker.onerror = () => reject(unreadable());
          worker.postMessage({ buffer, kind }, [buffer]);
        });
      })(), new Promise((_, reject) => { timeout = setTimeout(() => { expired = true; reject(unreadable()); }, 20000); })]);
      checkDimensions(decoded.width, decoded.height);
      const canvas = document.createElement('canvas');
      canvas.width = decoded.width; canvas.height = decoded.height;
      try { canvas.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(decoded.buffer), decoded.width, decoded.height), 0, 0); }
      catch (error) { canvas.width = canvas.height = 1; throw error; }
      return { source: canvas, width: canvas.width, height: canvas.height, orientation: decoded.orientation, close: () => { canvas.width = canvas.height = 1; } };
    } finally { clearTimeout(timeout); if (worker) worker.terminate(); }
  }
  function draw(canvas, source, longSide) {
    const swapped = source.orientation >= 5 && source.orientation <= 8;
    const ratio = Math.min(1, longSide / Math.max(source.width, source.height));
    const w = Math.max(1, Math.round(source.width * ratio)), h = Math.max(1, Math.round(source.height * ratio));
    canvas.width = swapped ? h : w; canvas.height = swapped ? w : h;
    const ctx = canvas.getContext('2d');
    // TIFF orientation is separate; native decode and libheif apply theirs.
    const transforms = { 2: [-1,0,0,1,w,0], 3: [-1,0,0,-1,w,h], 4: [1,0,0,-1,0,h], 5: [0,1,1,0,0,0], 6: [0,1,-1,0,h,0], 7: [0,-1,-1,0,h,w], 8: [0,-1,1,0,0,w] };
    if (transforms[source.orientation]) ctx.setTransform(...transforms[source.orientation]);
    ctx.drawImage(source.source, 0, 0, w, h);
  }
  const blobFrom = (canvas, type, quality) => new Promise((resolve, reject) => canvas.toBlob(blob => blob ? resolve(blob) : reject(new Error('Could not make a small copy of this photo.')), type, quality));
  async function shrink(file, options = {}) {
    const byteCeiling = Math.min(950000, Math.floor((options.maxBytes || 1000000) * 0.95));
    const byteTarget = Math.min(300000, byteCeiling);
    const longSide = Math.min(1600, options.maxDimension || 2048);
    const decoded = await decode(file, options);
    const canvas = document.createElement('canvas');
    try {
      try { draw(canvas, decoded, longSide); } finally { decoded.close(); }
      let type = 'image/webp';
      let blob = await blobFrom(canvas, type, 0.8);
      if (blob.type !== type) { type = 'image/jpeg'; blob = await blobFrom(canvas, type, 0.8); }
      for (const quality of [0.7, 0.6, 0.5, 0.4]) {
        if (blob.size <= byteTarget) break;
        blob = await blobFrom(canvas, type, quality);
      }
      // Difficult noise-heavy images may need fewer pixels as well.
      while (blob.size > byteTarget && Math.max(canvas.width, canvas.height) > Math.min(640, longSide / 2)) {
        const smaller = document.createElement('canvas');
        smaller.width = Math.max(1, Math.round(canvas.width * 0.8));
        smaller.height = Math.max(1, Math.round(canvas.height * 0.8));
        smaller.getContext('2d').drawImage(canvas, 0, 0, smaller.width, smaller.height);
        canvas.width = smaller.width; canvas.height = smaller.height;
        canvas.getContext('2d').drawImage(smaller, 0, 0);
        smaller.width = smaller.height = 1;
        blob = await blobFrom(canvas, type, 0.7);
      }
      if (blob.type !== type || blob.size >= byteCeiling) throw new Error('Could not make this photo small enough. Try another copy.');
      return { blob, ext: type === 'image/webp' ? '.webp' : '.jpg' };
    } finally { canvas.width = canvas.height = 1; }
  }
  root.SHPhotos = { shrink, slug, dataURL, codecKind, occupiedPaths, photoPath, digest };
})(globalThis);
