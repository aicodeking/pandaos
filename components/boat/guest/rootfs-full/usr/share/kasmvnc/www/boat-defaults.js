// Seeds the KasmVNC client's defaults before it starts; a choice the user already
// saved wins. The stock defaults (video_quality 2) ask the server for 24 fps at
// 960x540. Preset 3 asks for 60 fps at 1080p; stream_mode -1027 is software H.264,
// which the client decodes with WebCodecs. See guest/README.md for measurements.
(() => {
  const defaults = {video_quality: "3", stream_mode: "-1027"};
  for (const [k, v] of Object.entries(defaults)) {
    if (localStorage.getItem(k) === null) localStorage.setItem(k, v);
  }
})();
