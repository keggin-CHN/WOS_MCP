from PIL import Image
import sys

def convert_to_ico(png_path, ico_path):
    img = Image.open(png_path)
    img.save(ico_path, format='ICO', sizes=[(256, 256)])
    print("Icon generated successfully at:", ico_path)

if __name__ == "__main__":
    convert_to_ico(sys.argv[1], sys.argv[2])
