import { Settings } from 'lucide-react';
import { useCallback } from 'react';

export default function ComposeEdgeBadge({
  composeStrategy,
  onClick,
  position,
}: {
  composeStrategy?: string | null;
  onClick: () => void;
  position: { x: number; y: number };
}) {
  // Map strategy to color
  const colorFor = useCallback((s?: string | null) => {
    switch ((s || 'default').toLowerCase()) {
      case 'override':
        return { fill: '#10b981', stroke: '#064e3b' }; // emerald
      case 'merge_chat_histories':
        return { fill: '#0ea5e9', stroke: '#075985' }; // sky
      case 'append_string_to_chat_history':
        return { fill: '#a78bfa', stroke: '#5b21b6' }; // violet
      default:
        return { fill: '#64748b', stroke: '#334155' }; // slate
    }
  }, []);

  const { fill, stroke } = colorFor(composeStrategy);

  // Get short label for strategy
  const getShortLabel = (strategy?: string | null) => {
    if (!strategy) return 'default';

    const shortForms = {
      override: 'OVR',
      merge_chat_histories: 'MERGE',
      append_string_to_chat_history: 'APPEND',
    };

    return (
      shortForms[strategy as keyof typeof shortForms] || strategy.substring(0, 6).toUpperCase()
    );
  };

  return (
    <g
      transform={`translate(${position.x}, ${position.y})`}
      onClick={onClick}
      style={{ cursor: 'pointer' }}
      className="compose-edge-badge"
      opacity={1}>
      {/* chip background */}
      <rect
        x={-40}
        y={-12}
        width={80}
        height={24}
        rx={12}
        fill={fill}
        stroke={stroke}
        strokeWidth={1.5}
        filter="url(#shadow-filter)"
      />

      {/* shadow filter definition */}
      <defs>
        <filter id="shadow-filter" x="-20%" y="-20%" width="140%" height="140%">
          <feGaussianBlur in="SourceAlpha" stdDeviation="1" />
          <feOffset dx="0" dy="1" result="offsetblur" />
          <feFlood floodColor="rgba(0,0,0,0.15)" />
          <feComposite in2="offsetblur" operator="in" />
          <feMerge>
            <feMergeNode />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>

      {/* icon */}
      <foreignObject x={-36} y={-10} width={18} height={18} pointerEvents="none">
        <div className="flex items-center justify-center" style={{ width: 18, height: 18 }}>
          <Settings width={12} height={12} color="white" />
        </div>
      </foreignObject>

      {/* text */}
      <text
        x={-18}
        y={4}
        textAnchor="middle"
        fontSize="9"
        fontWeight={600}
        fill="white"
        pointerEvents="none">
        {getShortLabel(composeStrategy)}
      </text>

      {/* tooltip indicator */}
      <circle cx={24} cy={0} r={3} fill="white" opacity={0.7} pointerEvents="none" />
    </g>
  );
}
