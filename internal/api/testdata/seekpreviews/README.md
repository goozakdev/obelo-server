# Seek preview fixture

Frozen wire fixture for ADR-0070 / `docs/api-contract.md` "Seek previews — frozen contract".
`manifest.json` is pinned byte-for-byte by `seek_previews_wire_test.go`; `0.jpg` and `1.jpg`
are its two 10x8 tile sheets (3200x1440), 150 frames from a synthetic ~1497 s source at one
frame per 10 s (every 10th frame of a 1 fps source; ffmpeg's `fps=1/10` filter starts at 4 s here, so it is not used). Cells 70-79 of sheet 1 are unused and solid black.

Frame content: ffmpeg `testsrc2` (shows its own frame counter, so a wrong interval is
visible), with a 32x32 top-left patch painted neutral grey at limited-range luma `16+219*T/1497` (full-range 255*T/1497 once decoded) (T = source seconds) so
the test can check cell *i* is the frame at exactly *i*x10 s.

Regenerate (ffmpeg, no drawtext needed), from this directory:

```
ffmpeg -y -f lavfi -i "testsrc2=size=320x180:rate=1:duration=1497" \
  -vf "geq=lum='if(lt(X,32)*lt(Y,32),16+219*T/1497,lum(X,Y))':cb='if(lt(X,32)*lt(Y,32),128,cb(X,Y))':cr='if(lt(X,32)*lt(Y,32),128,cr(X,Y))',select='not(mod(n,10))',setpts=N/TB,tile=10x8:nb_frames=80:color=black" \
  -q:v 31 -start_number 0 %d.jpg
```
