import { Link } from 'react-router-dom';
import { Compass } from 'lucide-react';
import { Button, Empty } from '../components/ui';

export function NotFound() {
  return (
    <Empty
      title="Page not found"
      description="That management page does not exist."
      icon={<Compass size={28} />}
      action={
        <Link to="/">
          <Button variant="secondary">Back to Launchpad</Button>
        </Link>
      }
    />
  );
}
