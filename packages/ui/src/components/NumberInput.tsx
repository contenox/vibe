import { Input } from "./Input";

interface NumberInputProps {
  value: number | string;
  onChange: (value: number) => void;
  min?: number;
  max?: number;
  step?: number;
  placeholder?: string;
  className?: string;
}

export default function NumberInput({
  value,
  onChange,
  min,
  max,
  step = 1,
  placeholder,
  className,
}: NumberInputProps) {
  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const numValue = parseFloat(e.target.value);
    if (!isNaN(numValue)) {
      onChange(numValue);
    } else if (e.target.value === "") {
      onChange(0);
    }
  };

  return (
    <Input
      type="number"
      value={value}
      onChange={handleChange}
      min={min}
      max={max}
      step={step}
      placeholder={placeholder}
      className={className}
    />
  );
}
