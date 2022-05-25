import styled from "styled-components";
// eslint-disable-next-line
import { colors, fontSizes } from "../typedefs/styled";

export interface TextProps {
  className?: string;
  size?: keyof typeof fontSizes;
  bold?: boolean;
  semiBold?: boolean;
  capitalize?: boolean;
  italic?: boolean;
  color?: keyof typeof colors;
  uppercase?: boolean;
}

function textTransform(props) {
  if (props.capitalize) {
    return "capitalize";
  }

  if (props.uppercase) {
    return "uppercase";
  }

  return "none";
}

const Text = styled.span<TextProps>`
  font-family: ${(props) => props.theme.fontFamilies.regular};
  font-size: ${(props) => props.theme.fontSizes[props.size]};
  font-weight: ${(props) => {
    if (props.bold) return "800";
    else if (props.semiBold) return "600";
    else return "400";
  }};
  text-transform: ${textTransform};

  font-style: ${(props) => (props.italic ? "italic" : "normal")};
  color: ${(props) => props.theme.colors[props.color as any]};
`;

Text.defaultProps = {
  size: "medium",
};

export default Text;
