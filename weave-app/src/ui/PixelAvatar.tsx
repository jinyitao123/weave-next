interface PixelAvatarProps {
  seed: string;
  size?: number;
  className?: string;
}

// PixelAvatar renders a deterministic two-tone initial avatar. The hue is
// derived from the seed so the same agent always gets the same color without
// any server round-trip or random state.
export function PixelAvatar({ seed, size = 28, className }: PixelAvatarProps) {
  let hash = 0;
  for (let index = 0; index < seed.length; index++) {
    hash = (hash * 31 + seed.charCodeAt(index)) | 0;
  }
  const hue = ((hash % 360) + 360) % 360;
  const initial = (seed.trim().replace(/^__/, "").charAt(0) || "?").toUpperCase();
  return (
    <span
      className={className}
      aria-hidden="true"
      style={{
        display: "inline-flex",
        alignItems: "center",
        justifyContent: "center",
        width: size,
        height: size,
        borderRadius: Math.max(4, Math.round(size / 7)),
        background: `hsl(${hue} 45% 88%)`,
        color: `hsl(${hue} 55% 32%)`,
        fontSize: Math.round(size / 2),
        fontWeight: 600,
        flexShrink: 0,
        userSelect: "none",
      }}
    >
      {initial}
    </span>
  );
}
