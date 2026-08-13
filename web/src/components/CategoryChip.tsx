export interface CategoryChipProps {
  category?: string;
}

/**
 * A run's flat category tag (task t3/t5) as a small chip, rendered
 * wherever a run's name/hint shows — RunsList, RunCard, JobsTable, RunView.
 * Renders nothing when the run carries no category, rather than an empty
 * chip or an invented "uncategorized" label.
 */
export function CategoryChip({ category }: CategoryChipProps) {
  if (!category) return null;
  return (
    <span className="category-chip" data-category={category}>
      {category}
    </span>
  );
}

export default CategoryChip;
