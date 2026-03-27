import { ResponsiveContainer, LineChart, Line } from 'recharts'

export function SparklineChart({
  data,
  dataKey = 'value',
  color = 'var(--accent)',
}) {
  if (!data || data.length === 0) return null
  return (
    <ResponsiveContainer width="100%" height={40}>
      <LineChart data={data}>
        <Line type="monotone" dataKey={dataKey} stroke={color}
          dot={false} strokeWidth={1.5} />
      </LineChart>
    </ResponsiveContainer>
  )
}
