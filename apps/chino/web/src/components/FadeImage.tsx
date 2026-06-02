import { useState, type ImgHTMLAttributes } from 'react';

type FadeImageProps = ImgHTMLAttributes<HTMLImageElement>;

/**
 * <img> that stays invisible until the browser has decoded the full
 * image, then fades in over 500ms. On slow connections this replaces
 * the partial top-to-bottom paint of a JPEG/AVIF with a clean
 * placeholder→image transition — the wait feels like an intentional
 * UX touch instead of a network hiccup.
 *
 * Behaviour notes:
 *  - The placeholder is whatever sits behind the <img> in the parent
 *    (typically a dark `bg-[#161b22]` block). FadeImage doesn't draw
 *    its own — it would either fight the parent or require knowing
 *    the parent's intrinsic size.
 *  - Cached images load synchronously enough that onLoad fires before
 *    the first paint; the fade is then sub-perceivable.
 *  - onError marks the image "loaded" too so a broken URL doesn't
 *    leave the element invisible — that lets the alt text show and
 *    any parent onError fallback (e.g. EpisodesList backdrop →
 *    poster swap) take over.
 */
export function FadeImage({ className = '', onLoad, onError, ...rest }: FadeImageProps) {
  const [loaded, setLoaded] = useState(false);
  return (
    <img
      {...rest}
      onLoad={(e) => { setLoaded(true); onLoad?.(e); }}
      onError={(e) => { setLoaded(true); onError?.(e); }}
      className={`${className} transition-opacity duration-500 ease-out ${loaded ? 'opacity-100' : 'opacity-0'}`}
    />
  );
}
